// Package transport 负责请求执行：worker 池、重试、计时、节流与错误分类。
// 它不认识探针与协议 —— 输入是「怎么发」，输出是 rawdata.Record 的传输字段。
package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/chaterm/token-verifier/internal/adapter"
	"github.com/chaterm/token-verifier/internal/rawdata"
)

// Options 运行时参数（config.Runtime 的子集）。
type Options struct {
	MaxConcurrency int
	TimeoutSec     int
	MinIntervalMs  int
	MaxAttempts    int // 含首次
	// 重试退避：第 attempt 次重试前等待 min(BackoffBaseMs << attempt, BackoffMaxMs)。
	BackoffBaseMs int
	BackoffMaxMs  int
}

// Task 一次逻辑请求。Render 由 transport 调 adapter 完成。
type Task struct {
	Adapter       adapter.Adapter
	BaseURL       string
	APIKey        string
	Logical       adapter.LogicalRequest
	Thinking      adapter.ThinkingBlock
	TransportMode string // stream | non_stream
	RedactSecret  string // error_detail 脱敏用的密钥明文
}

// Result 单次尝试的结果：要么成功响应，要么分类后的错误。
// 计时字段由 transport 统一填写。
type Result struct {
	Response  adapter.Response
	HTTPCode  int    // 0 = 未收到响应
	ErrorKind string // 空 = 成功
	ErrorMsg  string // 已脱敏的原始错误摘要
	LatencyMs int
	TtftMs    int // -1 = 非流式或未收到内容 chunk
	Tps       float64
}

// Client 执行任务的 HTTP 客户端封装。
type Client struct {
	opts   Options
	http   *http.Client
	mu     sync.Mutex
	lastAt time.Time // 全局请求间隔节流
}

// New 按 Options 构造。真实缺省值的唯一来源是 config.ApplyDefaults；
// 这里只做防御性 clamp，保证未经配置直接构造（如测试）时仍可用。
func New(opts Options) *Client {
	if opts.MaxConcurrency < 1 {
		opts.MaxConcurrency = 1
	}
	if opts.TimeoutSec < 1 {
		opts.TimeoutSec = 1
	}
	if opts.MaxAttempts < 1 {
		opts.MaxAttempts = 1
	}
	if opts.BackoffBaseMs < 0 {
		opts.BackoffBaseMs = 0
	}
	return &Client{
		opts: opts,
		http: &http.Client{
			Timeout: time.Duration(opts.TimeoutSec) * time.Second,
			// 不跟随重定向：base_url 由使用者填入，端点本身可能就是恶意的。
			// Go 默认策略在跨主机重定向时只剥离 Authorization 等标准头，
			// x-api-key 这类自定义认证头会被原样转发到 Location 指向的
			// 任意主机（实测确认）。3xx 作为最终响应交给下面的状态分类，
			// 记为 http_3xx 错误。
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// backoffFor 第 attempt 次重试前的退避时长：指数增长、封顶
// （防移位溢出与失控等待）。max 小于 base 时以 base 为准。
func (c *Client) backoffFor(attempt int) time.Duration {
	base, max := c.opts.BackoffBaseMs, c.opts.BackoffMaxMs
	if base < 0 {
		base = 0
	}
	if max < base {
		max = base
	}
	if attempt > 30 {
		attempt = 30
	}
	ms := base << attempt
	if ms > max {
		ms = max
	}
	return time.Duration(ms) * time.Millisecond
}

// Do 执行一次请求（不含重试）。约好：任何 error 都已被分类到 Result.ErrorKind。
func (c *Client) Do(ctx context.Context, t Task) Result {
	// 全局最小间隔：持锁预约发送时刻，锁外等待。多个 goroutine 并发时
	// 依次预约到不同时刻，保证间隔严格成立。
	if c.opts.MinIntervalMs > 0 {
		c.mu.Lock()
		now := time.Now()
		earliest := c.lastAt.Add(time.Duration(c.opts.MinIntervalMs) * time.Millisecond)
		if earliest.Before(now) {
			earliest = now
		}
		c.lastAt = earliest
		c.mu.Unlock()
		if wait := time.Until(earliest); wait > 0 {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return Result{ErrorKind: "connection", ErrorMsg: ctx.Err().Error()}
			}
		}
	}

	req, err := t.Adapter.Render(t.BaseURL, t.APIKey, t.Logical, t.Thinking)
	if err != nil {
		return Result{ErrorKind: "protocol", ErrorMsg: redact(err.Error(), t.RedactSecret)}
	}

	start := time.Now()
	resp, err := c.http.Do(req.WithContext(ctx))
	if err != nil {
		kind := "connection"
		if isTimeout(err) {
			kind = "timeout"
		}
		return Result{ErrorKind: kind, ErrorMsg: redact(err.Error(), t.RedactSecret), LatencyMs: msSince(start)}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body := readSnippet(resp.Body)
		_ = resp.Body.Close()
		kind := "http_4xx"
		switch {
		case resp.StatusCode >= 500:
			kind = "http_5xx"
		case resp.StatusCode >= 300 && resp.StatusCode < 400:
			// CheckRedirect 已拒绝跟随：重定向本身是证据（端点接线异常或
			// 恶意转发），单独归类，不参与重试。
			kind = "http_3xx"
		}
		return Result{
			HTTPCode:  resp.StatusCode,
			ErrorKind: kind,
			ErrorMsg:  redact(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, body), t.RedactSecret),
			LatencyMs: msSince(start),
		}
	}

	// 成功路径：按流式与否解析并计时
	if t.TransportMode == "stream" {
		return c.parseStream(t, resp, start)
	}
	r, err := t.Adapter.ParseOnce(resp)
	lat := msSince(start)
	if err != nil {
		return Result{
			HTTPCode:  resp.StatusCode,
			ErrorKind: errKindOf(err),
			ErrorMsg:  redact(err.Error(), t.RedactSecret),
			LatencyMs: lat,
		}
	}
	// 非流式：ttft/tps 语义为 null（不是 0），用 -1 标记由调用方转 null
	return Result{Response: r, HTTPCode: resp.StatusCode, LatencyMs: lat, TtftMs: -1}
}

// parseStream 流式解析：记录首内容 chunk 时间（ttft）与末 chunk 时间（tps）。
func (c *Client) parseStream(t Task, resp *http.Response, start time.Time) Result {
	var firstContent, lastChunk time.Time
	var completionTokens int
	r, err := t.Adapter.ParseStream(resp, func(ch adapter.Chunk) {
		now := time.Now()
		if ch.Delta != "" && firstContent.IsZero() {
			firstContent = now
		}
		if ch.Delta != "" || ch.Done {
			lastChunk = now
		}
		if ch.Usage != nil {
			completionTokens = ch.Usage.Completion
		}
	})
	lat := msSince(start)
	if err != nil {
		return Result{
			HTTPCode:  resp.StatusCode,
			ErrorKind: errKindOf(err),
			ErrorMsg:  redact(err.Error(), t.RedactSecret),
			LatencyMs: lat,
			TtftMs:    -1,
		}
	}
	if completionTokens == 0 {
		completionTokens = r.Usage.Completion
	}
	out := Result{Response: r, HTTPCode: resp.StatusCode, LatencyMs: lat, TtftMs: -1}
	if !firstContent.IsZero() {
		ttft := int(firstContent.Sub(start).Milliseconds())
		out.TtftMs = ttft
		if !lastChunk.Before(firstContent) {
			elapsed := lastChunk.Sub(firstContent).Seconds()
			if elapsed <= 0 {
				// 首末 chunk 落在同一毫秒内：按至少 1ms 计，避免除零
				elapsed = 0.001
			}
			out.Tps = float64(completionTokens) / elapsed
		}
	}
	return out
}

// Runner 并发执行任务队列，回调按完成顺序上报。
// 每个任务最多尝试 MaxAttempts 次；每次尝试都回调一次（含失败尝试），
// 调用方据此写多条 record（attempt 递增，不覆盖 —— 重试不能把错误率洗白）。
type Runner struct {
	client *Client
}

func NewRunner(c *Client) *Runner { return &Runner{client: c} }

// Run 执行全部任务并等待完成。任务间并发（受 MaxConcurrency 限制），
// ctx 取消后未开始的任务不再发出，已在途的自然结束。
func (r *Runner) Run(ctx context.Context, tasks []Task, onAttempt func(idx int, attempt int, res Result)) {
	sem := make(chan struct{}, r.client.opts.MaxConcurrency)
	var wg sync.WaitGroup
	for i, t := range tasks {
		wg.Add(1)
		go func(idx int, t Task) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			for attempt := 0; attempt < r.client.opts.MaxAttempts; attempt++ {
				res := r.client.Do(ctx, t)
				onAttempt(idx, attempt, res)
				if res.ErrorKind == "" {
					return
				}
				// 3xx/4xx（端点接线、能力/参数类）重试无意义，直接判失败
				if res.ErrorKind == "http_3xx" || res.ErrorKind == "http_4xx" ||
					res.ErrorKind == "protocol" || res.ErrorKind == "parse" {
					return
				}
				if attempt == r.client.opts.MaxAttempts-1 {
					return // 最后一次尝试，无需退避
				}
				select {
				case <-time.After(r.client.backoffFor(attempt)):
				case <-ctx.Done():
					return
				}
			}
		}(i, t)
	}
	wg.Wait()
}

// —— 错误分类 ——

// isTimeout 判断是否超时（客户端超时或上下文截止）。
func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return true
	}
	// http.Client 超时被包成 url.Error
	var ne net.Error
	if errors.As(err, &ne) {
		return ne.Timeout()
	}
	msg := err.Error()
	return strings.Contains(msg, "Client.Timeout") || strings.Contains(msg, "context deadline exceeded")
}

// errKindOf 把 adapter 层错误映射到 error_kind。
func errKindOf(err error) string {
	var pe *adapter.ProtocolError
	if errors.As(err, &pe) {
		return pe.Kind // protocol | parse
	}
	return "parse"
}

// msSince 毫秒耗时。
func msSince(t time.Time) int { return int(time.Since(t).Milliseconds()) }

// readSnippet 读一小段响应体做错误摘要。
func readSnippet(r io.Reader) string {
	buf := make([]byte, 512)
	n, _ := r.Read(buf)
	s := string(buf[:n])
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// redact 把密钥字面量替换为 ***，error_detail 落盘前必须过这里。
func redact(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "***")
}

// maxErrorDetailBytes error_detail 落盘前的字节上限（含截断标记）。恶意端点
// 可在 HTTP 200 响应体内嵌巨大 error.message；rawdata 读回侧 bufio.Scanner
// 单行上限是 16MB，超限会让证据文件（含当次采集收尾的读回）永久不可读。
const maxErrorDetailBytes = 4096

// capDetail 把错误摘要截断到 maxErrorDetailBytes，截断点回退到 rune 边界，
// 避免切碎 UTF-8 多字节字符。
func capDetail(s string) string {
	if len(s) <= maxErrorDetailBytes {
		return s
	}
	cut := maxErrorDetailBytes - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// —— record 组装 ——

// FillTransport 把 Result 写进 Record 的传输字段。
// 非流式的 ttft/tps 写 nil（不是 0）：占位 0 会让字段永久失去意义。
// 未收到响应（连接失败/超时）时 http_code 同样为 nil —— SPEC-RAWDATA §4。
func FillTransport(rec *rawdata.Record, mode string, res Result) {
	rec.TransportMode = mode
	rec.Status = "success"
	rec.LatencyMs = res.LatencyMs
	if res.HTTPCode != 0 {
		code := res.HTTPCode
		rec.HTTPCode = &code
	}
	if res.ErrorKind != "" {
		rec.Status = "error"
		kind := res.ErrorKind
		rec.ErrorKind = &kind
		detail := capDetail(res.ErrorMsg)
		rec.ErrorDetail = &detail
		return
	}
	if mode == "stream" && res.TtftMs >= 0 {
		ttft := res.TtftMs
		rec.TtftMs = &ttft
	}
	if mode == "stream" && res.Tps > 0 {
		tps := res.Tps
		rec.Tps = &tps
	}
}
