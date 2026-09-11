package transport

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chaterm/token-verifier/internal/adapter"
	"github.com/chaterm/token-verifier/internal/rawdata"
)

// fakeAdapter 直连 httptest 假端点，绕开协议细节。
type fakeAdapter struct {
	id string
}

func (f fakeAdapter) ID() string { return f.id }
func (f fakeAdapter) Render(baseURL, apiKey string, lr adapter.LogicalRequest, tb adapter.ThinkingBlock) (*http.Request, error) {
	req, err := http.NewRequest(http.MethodPost, baseURL+"/x", nil)
	if err != nil {
		return nil, err
	}
	return req, nil
}
func (f fakeAdapter) ParseOnce(resp *http.Response) (adapter.Response, error) {
	return adapter.Response{Content: "42", Usage: adapter.Usage{Prompt: 10, Completion: 3}}, nil
}
func (f fakeAdapter) ParseStream(resp *http.Response, onChunk func(adapter.Chunk)) (adapter.Response, error) {
	onChunk(adapter.Chunk{Delta: "4"})
	onChunk(adapter.Chunk{Delta: "2"})
	onChunk(adapter.Chunk{Done: true})
	return adapter.Response{Content: "42", Usage: adapter.Usage{Prompt: 10, Completion: 3}}, nil
}

func TestDoNonStreamSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()
	c := New(Options{MaxConcurrency: 1, TimeoutSec: 5, MaxAttempts: 1})
	res := c.Do(context.Background(), Task{Adapter: fakeAdapter{}, BaseURL: srv.URL, TransportMode: "non_stream", Logical: adapter.LogicalRequest{}})
	if res.ErrorKind != "" {
		t.Fatalf("err = %v", res.ErrorMsg)
	}
	if res.TtftMs != -1 {
		t.Errorf("非流式 ttft 应为 -1（转 null），得到 %d", res.TtftMs)
	}
	if res.Tps != 0 {
		t.Errorf("非流式 tps 应为 0（转 null）")
	}
}

func TestDoStreamTiming(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()
	c := New(Options{MaxConcurrency: 1, TimeoutSec: 5, MaxAttempts: 1})
	res := c.Do(context.Background(), Task{Adapter: fakeAdapter{}, BaseURL: srv.URL, TransportMode: "stream", Logical: adapter.LogicalRequest{}})
	if res.TtftMs < 0 {
		t.Errorf("流式应记录 ttft")
	}
	if res.Tps <= 0 {
		t.Errorf("流式应记录 tps，得到 %v", res.Tps)
	}
}

func TestDoTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(200)
	}))
	defer srv.Close()
	c := New(Options{MaxConcurrency: 1, TimeoutSec: 1, MaxAttempts: 1})
	// 用短超时客户端
	c.http.Timeout = 50 * time.Millisecond
	res := c.Do(context.Background(), Task{Adapter: fakeAdapter{}, BaseURL: srv.URL, TransportMode: "non_stream"})
	if res.ErrorKind != "timeout" {
		t.Errorf("ErrorKind = %q, want timeout", res.ErrorKind)
	}
}

func TestDoHTTP4xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		fmt.Fprint(w, `{"error":{"message":"bad"}}`)
	}))
	defer srv.Close()
	c := New(Options{MaxAttempts: 1})
	res := c.Do(context.Background(), Task{Adapter: fakeAdapter{}, BaseURL: srv.URL, TransportMode: "non_stream"})
	if res.ErrorKind != "http_4xx" || res.HTTPCode != 400 {
		t.Errorf("res = %+v", res)
	}
}

func TestDoRedactSecret(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		fmt.Fprint(w, "key sk-secret-123 leaked")
	}))
	defer srv.Close()
	c := New(Options{MaxAttempts: 1})
	res := c.Do(context.Background(), Task{Adapter: fakeAdapter{}, BaseURL: srv.URL, TransportMode: "non_stream", RedactSecret: "sk-secret-123"})
	if res.ErrorMsg != "HTTP 400: key *** leaked" {
		t.Errorf("ErrorMsg = %q, want 密钥脱敏", res.ErrorMsg)
	}
}

func TestRunnerRetry(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) < 3 {
			w.WriteHeader(500) // 前两次 500，第三次成功
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	c := New(Options{MaxConcurrency: 1, TimeoutSec: 5, MaxAttempts: 3})
	// 缩短退避：这里只验证重试次数
	r := NewRunner(c)
	var attempts int
	r.Run(context.Background(), []Task{{Adapter: fakeAdapter{}, BaseURL: srv.URL, TransportMode: "non_stream"}}, func(idx, attempt int, res Result) {
		attempts++
	})
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3（500 可重试）", attempts)
	}
}

func TestRunnerNoRetryOn4xx(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		w.WriteHeader(400)
	}))
	defer srv.Close()
	c := New(Options{MaxConcurrency: 1, TimeoutSec: 5, MaxAttempts: 3})
	r := NewRunner(c)
	var attempts int
	r.Run(context.Background(), []Task{{Adapter: fakeAdapter{}, BaseURL: srv.URL, TransportMode: "non_stream"}}, func(idx, attempt int, res Result) {
		attempts++
	})
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1（4xx 不重试）", attempts)
	}
}

func TestMinInterval(t *testing.T) {
	var mu sync.Mutex
	var times []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		times = append(times, time.Now())
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer srv.Close()
	c := New(Options{MaxConcurrency: 4, TimeoutSec: 5, MaxAttempts: 1, MinIntervalMs: 80})
	tasks := make([]Task, 3)
	for i := range tasks {
		tasks[i] = Task{Adapter: fakeAdapter{}, BaseURL: srv.URL, TransportMode: "non_stream"}
	}
	r := NewRunner(c)
	start := time.Now()
	r.Run(context.Background(), tasks, func(int, int, Result) {})
	elapsed := time.Since(start)
	// 3 个请求 × 80ms 间隔 ≈ 至少 160ms（首请求立即发）
	if elapsed < 150*time.Millisecond {
		t.Errorf("min_interval 未生效: elapsed = %v", elapsed)
	}
}

func TestFillTransportNulls(t *testing.T) {
	rec := &rawdata.Record{HTTPCode: new(int)}
	FillTransport(rec, "non_stream", Result{Response: adapter.Response{}, HTTPCode: 200, LatencyMs: 100, TtftMs: -1})
	if rec.Status != "success" {
		t.Errorf("status = %q", rec.Status)
	}
	if rec.TtftMs != nil || rec.Tps != nil {
		t.Errorf("非流式 ttft/tps 必须是 nil，不是 0")
	}

	rec2 := &rawdata.Record{HTTPCode: new(int)}
	ttft := 120
	tps := 45.0
	FillTransport(rec2, "stream", Result{HTTPCode: 200, LatencyMs: 300, TtftMs: ttft, Tps: tps})
	if rec2.TtftMs == nil || *rec2.TtftMs != 120 || rec2.Tps == nil || *rec2.Tps != 45.0 {
		t.Errorf("stream rec = %+v", rec2)
	}

	rec3 := &rawdata.Record{HTTPCode: new(int)}
	FillTransport(rec3, "non_stream", Result{HTTPCode: 500, ErrorKind: "http_5xx", ErrorMsg: "x", LatencyMs: 10})
	if rec3.Status != "error" || *rec3.ErrorKind != "http_5xx" {
		t.Errorf("error rec = %+v", rec3)
	}
}

func TestBackoffFor(t *testing.T) {
	c := New(Options{BackoffBaseMs: 500, BackoffMaxMs: 30000})
	if got := c.backoffFor(0); got != 500*time.Millisecond {
		t.Errorf("attempt0 = %v, want 500ms", got)
	}
	if got := c.backoffFor(1); got != 1000*time.Millisecond {
		t.Errorf("attempt1 = %v, want 1000ms", got)
	}
	if got := c.backoffFor(2); got != 2000*time.Millisecond {
		t.Errorf("attempt2 = %v, want 2000ms", got)
	}
	// 封顶
	if got := c.backoffFor(20); got != 30000*time.Millisecond {
		t.Errorf("attempt20 = %v, want 封顶 30000ms", got)
	}
	// 自定义参数生效
	c2 := New(Options{BackoffBaseMs: 10, BackoffMaxMs: 25})
	if got := c2.backoffFor(0); got != 10*time.Millisecond {
		t.Errorf("custom attempt0 = %v, want 10ms", got)
	}
	if got := c2.backoffFor(3); got != 25*time.Millisecond {
		t.Errorf("custom attempt3 = %v, want 封顶 25ms", got)
	}
}

func TestRunnerRetryUsesBackoffConfig(t *testing.T) {
	// base=1ms：三次尝试的总退避应远小于默认 500+1000ms
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) < 3 {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	c := New(Options{MaxConcurrency: 1, TimeoutSec: 5, MaxAttempts: 3, BackoffBaseMs: 1, BackoffMaxMs: 2})
	r := NewRunner(c)
	start := time.Now()
	r.Run(context.Background(), []Task{{Adapter: fakeAdapter{}, BaseURL: srv.URL, TransportMode: "non_stream"}}, func(idx, attempt int, res Result) {})
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("退避配置未生效：耗时 %v，want < 2s", elapsed)
	}
}
