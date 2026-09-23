package collect

// 端到端测试：httptest 假端点 → collect.Run → 产出 rawData → 喂 compare。
// 不打真实 API。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/chaterm/token-verifier/internal/compare"
	"github.com/chaterm/token-verifier/internal/config"
	"github.com/chaterm/token-verifier/internal/logging"
	"github.com/chaterm/token-verifier/internal/rawdata"
	"github.com/chaterm/token-verifier/internal/suite"
)

// fakeOpenAI 假 openai-chat 端点：按探测到的 prompt 类型返回答案与 usage。
// mode 决定行为："same" 与基线一致；"swap-tokenizer" 改 usage.prompt_tokens。
type fakeOpenAI struct {
	mode string
}

func (f fakeOpenAI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &req)

	// 取 user 消息为题面（输出契约启用时 Messages[0] 可能是 system）
	prompt := ""
	if len(req.Messages) > 0 {
		prompt = req.Messages[len(req.Messages)-1].Content
		for _, m := range req.Messages {
			if m.Role == "user" {
				prompt = m.Content
				break
			}
		}
	}
	promptTokens := 50 + len(prompt)/4
	if f.mode == "swap-tokenizer" && containsCJK(prompt) {
		promptTokens += 999 // tokenizer 应能检测到
	}
	completion := fmt.Sprintf("%d", 1+rand.New(rand.NewSource(int64(len(prompt)))).Intn(100))

	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", completion)
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":3}}\n\n", promptTokens)
		fmt.Fprint(w, "data: [DONE]\n\n")
		return
	}
	w.WriteHeader(200)
	fmt.Fprintf(w, `{"choices":[{"message":{"content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":%d,"completion_tokens":3}}`, completion, promptTokens)
}

func containsCJK(s string) bool {
	for _, r := range s {
		if r > 0x2E80 {
			return true
		}
	}
	return false
}

// testConfig 一份完整可跑的采集配置。
func testConfig(t *testing.T, baseURL, suitePath string) *config.File {
	t.Helper()
	f := &config.File{
		Version: 1,
		Target: config.Target{
			BaseURL:   baseURL,
			APIKeyEnv: "TV_E2E_KEY",
			Model:     "fake-model",
			Protocols: []config.ProtocolConfig{
				{ID: "openai-chat", Weight: 1.0,
					Transport:    config.TransportMix{Stream: 0.5, NonStream: 0.5},
					Capabilities: config.Capabilities{Stream: true, Tools: true, Thinking: true},
					Thinking:     config.ThinkingBlock{Disabled: config.ThinkingMode{BodyOverrides: map[string]any{}}}},
			},
			Request: config.RequestConfig{TemperatureField: "temperature"},
		},
		Suite: config.SuiteRef{Path: suitePath},
		Probes: map[string]config.ProbeConfig{
			"onetoken": {Enabled: true, Repeats: 10, Temperature: f64p(1.0),
				MaxTokens: intptr(16), ContextBuckets: []int{0}},
			"tokenizer": {Enabled: true, Repeats: 1, Temperature: f64p(0.0),
				MaxTokens: intptr(16), ContextBuckets: []int{0}},
		},
		Runtime: config.Runtime{MaxConcurrency: 4, TimeoutSec: 10, MaxAttempts: 2, Seed: 42, RawLevel: "digest"},
	}
	// 与 config.Load 相同的缺省值处理
	f.ApplyDefaults()
	return f
}

func intptr(i int) *int { return &i }

// testSuite 写一份小题库并返回路径。
func testSuite(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "suite.yaml")
	content := `suite_version: v1
output_contract:
  field: answer
  system_prompt: '请只回答 JSON {"answer": "<your answer>"}'
probes:
  onetoken:
    normalize: number
    items:
      - id: ot.v1.001
        prompt: "说一个 1 到 100 的随机整数，只输出数字。"
      - id: ot.v1.002
        prompt: "随便说一个 1 到 100 的数字。"
  tokenizer:
    normalize: none
    items:
      - id: tk.v1.001
        prompt: "重复以下内容一次：こんにちは、🎉、ｕｌｌｗｉｄｔｈ。"
      - id: tk.v1.002
        prompt: "输出这段 JSON 的镜像：{\"a\": [1, 2, 3], \"b\": {\"c\": \"emoji 🚀\"}}"
      - id: tk.v1.003
        prompt: "重复以下内容一次：全角ＡＢＣ与中文混排文本。"
      - id: tk.v1.004
        prompt: "重复以下内容一次：한국어 텍스트와 日本語の混在。"
      - id: tk.v1.005
        prompt: "重复以下内容一次：𝔘𝔫𝔦𝔠𝔬𝔡𝔢 数学字母与 Σ∑ 符号。"
      - id: tk.v1.006
        prompt: "重复以下内容一次：嵌套结构 {\"x\": {\"y\": [true, null, 3.14]}}"
`
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCollectEndToEnd(t *testing.T) {
	os.Setenv("TV_E2E_KEY", "sk-e2e")
	defer os.Unsetenv("TV_E2E_KEY")

	suitePath := testSuite(t)

	// 采集 A：正常端点
	srvA := httptest.NewServer(fakeOpenAI{mode: "same"})
	defer srvA.Close()
	cfgA := testConfig(t, srvA.URL, suitePath)
	st, err := suite.Load(suitePath, "")
	if err != nil {
		t.Fatal(err)
	}
	outA := filepath.Join(t.TempDir(), "a.rawdata.jsonl.gz")
	resA, err := Run(context.Background(), cfgA, st, Options{OutputPath: outA})
	if err != nil {
		t.Fatalf("collect A: %v", err)
	}
	if resA.TotalRequests == 0 || resA.ErrorCount > 0 {
		t.Fatalf("resA = %+v", resA)
	}

	// 采集 B：同一端点（同分布）→ compare 应 pass
	outB := filepath.Join(t.TempDir(), "b.rawdata.jsonl.gz")
	cfgB := testConfig(t, srvA.URL, suitePath)
	resB, err := Run(context.Background(), cfgB, st, Options{OutputPath: outB})
	if err != nil {
		t.Fatalf("collect B: %v", err)
	}
	if resB.ErrorCount > 0 {
		t.Fatalf("resB = %+v", resB)
	}

	compareResult, err := compareRawdata(outA, outB)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range compareResult.Verdicts {
		if v.Verdict == "fail" {
			t.Errorf("同端点两份采集不应 fail: %s %v", v.ProbeID, v.Verdict)
		}
	}

	// 采集 C：换分词器端点 → tokenizer 应 fail
	srvC := httptest.NewServer(fakeOpenAI{mode: "swap-tokenizer"})
	defer srvC.Close()
	cfgC := testConfig(t, srvC.URL, suitePath)
	outC := filepath.Join(t.TempDir(), "c.rawdata.jsonl.gz")
	if _, err := Run(context.Background(), cfgC, st, Options{OutputPath: outC}); err != nil {
		t.Fatalf("collect C: %v", err)
	}
	compareResult2, err := compareRawdata(outA, outC)
	if err != nil {
		t.Fatal(err)
	}
	var tokenizerFail bool
	for _, v := range compareResult2.Verdicts {
		if v.ProbeID == "tokenizer" && v.Verdict == "fail" {
			tokenizerFail = true
		}
	}
	if !tokenizerFail {
		t.Errorf("换分词器端点 tokenizer 应 fail: %+v", compareResult2.Verdicts)
	}
}

func TestCollectDryRun(t *testing.T) {
	os.Setenv("TV_E2E_KEY", "sk-e2e")
	defer os.Unsetenv("TV_E2E_KEY")
	suitePath := testSuite(t)
	cfg := testConfig(t, "http://unused.example.com", suitePath)
	st, _ := suite.Load(suitePath, "")
	// 不联网：dry-run 不发请求
	res, err := Run(context.Background(), cfg, st, Options{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	// onetoken: 2 题 × 10 repeats = 20；tokenizer: 6 × 1 = 6
	if res.TotalRequests != 26 {
		t.Errorf("TotalRequests = %d, want 26", res.TotalRequests)
	}
}

func TestCollectResumeSkipsDone(t *testing.T) {
	os.Setenv("TV_E2E_KEY", "sk-e2e")
	defer os.Unsetenv("TV_E2E_KEY")
	suitePath := testSuite(t)
	srv := httptest.NewServer(fakeOpenAI{mode: "same"})
	defer srv.Close()
	st, _ := suite.Load(suitePath, "")

	out := filepath.Join(t.TempDir(), "out.rawdata.jsonl.gz")
	// 第一次跑：只跑 1 题（改配置 repeats=1 减少请求），模拟部分完成后中断
	cfg1 := testConfig(t, srv.URL, suitePath)
	cfg1.Probes["onetoken"] = config.ProbeConfig{Enabled: true, Repeats: 10, Temperature: f64p(1.0), MaxTokens: intptr(16), ContextBuckets: []int{0}}
	cfg1.Probes["tokenizer"] = config.ProbeConfig{Enabled: false}
	if _, err := Run(context.Background(), cfg1, st, Options{OutputPath: out}); err != nil {
		t.Fatal(err)
	}
	// 注意：cfg1 与 cfg 的 plan_digest 不同（probes 集合不同），
	// 为了测 resume，第二次必须用与第一次相同的配置
	res2, err := Run(context.Background(), cfg1, st, Options{OutputPath: out})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if res2.TotalRequests != 0 {
		t.Errorf("全部已完成的续跑应无新请求，得到 %d", res2.TotalRequests)
	}
}

// captureLog 构造写入 buffer 的 logger（复用 CLI 的格式与过滤）。
func captureLog(level slog.Level) (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return logging.Setup(&buf, level), &buf
}

func TestCollectLogging(t *testing.T) {
	os.Setenv("TV_E2E_KEY", "sk-e2e")
	defer os.Unsetenv("TV_E2E_KEY")
	suitePath := testSuite(t)
	srv := httptest.NewServer(fakeOpenAI{mode: "same"})
	defer srv.Close()
	cfg := testConfig(t, srv.URL, suitePath)
	cfg.Probes["tokenizer"] = config.ProbeConfig{} // 只留 onetoken，2 题 × 10 repeats
	st, _ := suite.Load(suitePath, "")

	// debug 级别：开始摘要 + 每条请求一行
	log, buf := captureLog(slog.LevelDebug)
	out := filepath.Join(t.TempDir(), "out.rawdata.jsonl.gz")
	if _, err := Run(context.Background(), cfg, st, Options{OutputPath: out, Log: log}); err != nil {
		t.Fatal(err)
	}
	s := buf.String()
	if !strings.Contains(s, "INFO") || !strings.Contains(s, "requests=20") {
		t.Errorf("应有 info 级开始摘要（requests=20）: %q", s)
	}
	if strings.Count(s, "DEBUG") < 20 {
		t.Errorf("debug 级应有每条请求的日志，DEBUG 行数 = %d", strings.Count(s, "DEBUG"))
	}
	if !strings.Contains(s, "probe=onetoken") {
		t.Errorf("debug 行应带探针标识: %q", s)
	}

	// info 级别：无 DEBUG 行
	log2, buf2 := captureLog(slog.LevelInfo)
	out2 := filepath.Join(t.TempDir(), "out2.rawdata.jsonl.gz")
	if _, err := Run(context.Background(), cfg, st, Options{OutputPath: out2, Log: log2}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf2.String(), "DEBUG") {
		t.Errorf("info 级别不应有 DEBUG 行: %q", buf2.String())
	}

	// Log 为 nil（库式调用）：不 panic、正常完成 —— 既有测试已覆盖，这里显式验证
	if _, err := Run(context.Background(), cfg, st, Options{OutputPath: filepath.Join(t.TempDir(), "out3.rawdata.jsonl.gz")}); err != nil {
		t.Fatalf("Log=nil: %v", err)
	}
}

// compareRawdata 读两个文件并跑 compare（阈值取宽松值，重点看 fail/pass）。
func compareRawdata(a, b string) (*compare.Result, error) {
	fileA, err := rawdata.Read(a)
	if err != nil {
		return nil, err
	}
	fileB, err := rawdata.Read(b)
	if err != nil {
		return nil, err
	}
	thresholds := map[string]float64{"onetoken": 0.9, "tokenizer": 0.01}
	return compare.Run(fileA, fileB, thresholds, nil)
}

// captureEP 记录收到的请求体，验证注入与温度路径真的到了端点。
type captureEP struct {
	mu     sync.Mutex
	bodies []map[string]any
}

func (c *captureEP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	c.mu.Lock()
	c.bodies = append(c.bodies, m)
	c.mu.Unlock()
	w.WriteHeader(200)
	fmt.Fprint(w, `{"choices":[{"message":{"content":"42"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":3}}`)
}

func TestCollectGlobalOverridesAndTempField(t *testing.T) {
	os.Setenv("TV_E2E_KEY", "sk-e2e")
	defer os.Unsetenv("TV_E2E_KEY")
	suitePath := testSuite(t)

	ep := &captureEP{}
	srv := httptest.NewServer(ep)
	defer srv.Close()

	cfg := testConfig(t, srv.URL, suitePath)
	// 全局 body_overrides + 嵌套温度路径
	cfg.Target.Request.BodyOverrides = map[string]any{"vendor_param": "vp-1"}
	cfg.Target.Request.TemperatureField = "generation_config.temperature"
	st, _ := suite.Load(suitePath, "")

	out := filepath.Join(t.TempDir(), "out.rawdata.jsonl.gz")
	if _, err := Run(context.Background(), cfg, st, Options{OutputPath: out}); err != nil {
		t.Fatal(err)
	}
	if len(ep.bodies) == 0 {
		t.Fatal("端点没有收到任何请求")
	}
	for _, b := range ep.bodies {
		if b["vendor_param"] != "vp-1" {
			t.Fatalf("全局 body_overrides 未进请求体: %v", b)
		}
		gc, ok := b["generation_config"].(map[string]any)
		if !ok {
			t.Fatalf("temperature_field 点路径未生效（无 generation_config）: %v", b)
		}
		if _, has := gc["temperature"]; !has {
			t.Fatalf("generation_config.temperature 缺失: %v", b)
		}
		if _, has := b["temperature"]; has {
			t.Fatalf("温度不应再写顶层 temperature: %v", b)
		}
	}
}

func TestCollectManifestShape(t *testing.T) {
	os.Setenv("TV_E2E_KEY", "sk-e2e")
	defer os.Unsetenv("TV_E2E_KEY")
	suitePath := testSuite(t)
	srv := httptest.NewServer(fakeOpenAI{mode: "same"})
	defer srv.Close()
	cfg := testConfig(t, srv.URL, suitePath)
	st, _ := suite.Load(suitePath, "")

	out := filepath.Join(t.TempDir(), "out.rawdata.jsonl.gz")
	if _, err := Run(context.Background(), cfg, st, Options{OutputPath: out}); err != nil {
		t.Fatal(err)
	}
	f, err := rawdata.Read(out)
	if err != nil {
		t.Fatal(err)
	}

	// capabilities：每协议直接一个能力对象（SPEC-RAWDATA §2）
	var caps map[string]map[string]any
	if err := json.Unmarshal(f.Manifest.Capabilities, &caps); err != nil {
		t.Fatal(err)
	}
	oc, ok := caps["openai-chat"]
	if !ok {
		t.Fatalf("capabilities 缺 openai-chat: %v", caps)
	}
	if _, has := oc["stream"]; !has {
		t.Errorf("capabilities[openai-chat] 应直接是能力对象: %v", oc)
	}

	// sampling：逐探针的 (protocol/transport) 整数计数
	var sampling map[string]map[string]int
	if err := json.Unmarshal(f.Manifest.Sampling, &sampling); err != nil {
		t.Fatal(err)
	}
	ot := sampling["onetoken"]
	// 2 题 × 10 repeats = 20，transport 0.5/0.5 → 每协议组合 10/10
	if ot["openai-chat/stream"] != 10 || ot["openai-chat/non_stream"] != 10 {
		t.Errorf("sampling[onetoken] = %v", ot)
	}
	tk := sampling["tokenizer"]
	// 6 题 × repeats=1，每题最大余数法切 [non_stream, stream] 平局取下标 0 → 全 non_stream
	if tk["openai-chat/non_stream"] != 6 || tk["openai-chat/stream"] != 0 {
		t.Errorf("sampling[tokenizer] = %v", tk)
	}

	// 密钥指纹只有 8 位 hex，明文不落盘
	if len(f.Manifest.Endpoint.APIKeyFingerprint) != 8 {
		t.Errorf("fingerprint = %q", f.Manifest.Endpoint.APIKeyFingerprint)
	}
	raw, _ := json.Marshal(f.Manifest)
	if bytes.Contains(raw, []byte("sk-e2e")) {
		t.Fatal("manifest 中出现密钥明文")
	}
}

func TestCollectSkippedDedup(t *testing.T) {
	os.Setenv("TV_E2E_KEY", "sk-e2e")
	defer os.Unsetenv("TV_E2E_KEY")
	suitePath := testSuite(t)
	srv := httptest.NewServer(fakeOpenAI{mode: "same"})
	defer srv.Close()
	cfg := testConfig(t, srv.URL, suitePath)
	// stream: false → 每题每档位都会产生同一条 stream_unsupported skip
	p := cfg.Target.Protocols[0]
	p.Capabilities.Stream = false
	cfg.Target.Protocols[0] = p
	st, _ := suite.Load(suitePath, "")

	out := filepath.Join(t.TempDir(), "out.rawdata.jsonl.gz")
	if _, err := Run(context.Background(), cfg, st, Options{OutputPath: out}); err != nil {
		t.Fatal(err)
	}
	f, err := rawdata.Read(out)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, s := range f.Manifest.Skipped {
		if s.Reason == "stream_unsupported" {
			n++
		}
	}
	// 8 题 × 1 档位，未去重会是 8 条；去重后每个 (probe, protocol) 一条
	if n != 2 {
		t.Errorf("stream_unsupported skip = %d 条, want 2（onetoken/tokenizer 各一条）", n)
	}
}

func TestCollectConnectionFailureHTTPCode(t *testing.T) {
	os.Setenv("TV_E2E_KEY", "sk-e2e")
	defer os.Unsetenv("TV_E2E_KEY")
	suitePath := testSuite(t)
	// 可达但全 400 的端点：不是致命错误，应落盘 error record
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		fmt.Fprint(w, `{"error":{"message":"bad"}}`)
	}))
	defer srv.Close()
	cfg := testConfig(t, srv.URL, suitePath)
	st, _ := suite.Load(suitePath, "")

	out := filepath.Join(t.TempDir(), "out.rawdata.jsonl.gz")
	res, err := Run(context.Background(), cfg, st, Options{OutputPath: out})
	if err != nil {
		t.Fatalf("全 400 不是致命错误，应正常落盘: %v", err)
	}
	if res.SuccessCount != 0 || res.ErrorCount == 0 {
		t.Errorf("res = %+v", res)
	}
	f, err := rawdata.Read(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Records) == 0 {
		t.Fatal("error record 应落盘")
	}
	if f.Records[0].HTTPCode == nil || *f.Records[0].HTTPCode != 400 {
		t.Errorf("http_code = %v, want 400", f.Records[0].HTTPCode)
	}
}

// 全 401 端点：采集本身"成功"（收到了 HTTP 响应），但用户必须能当场知道
// 为什么全失败 —— 而不是等 131 个请求跑完只看到「成功 0 / 失败 131」。
func TestCollectSurfacesEndpointErrors(t *testing.T) {
	os.Setenv("TV_E2E_KEY", "sk-e2e")
	defer os.Unsetenv("TV_E2E_KEY")
	suitePath := testSuite(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		fmt.Fprint(w, `{"error":{"message":"Incorrect API key provided: sk-e2e","code":"invalid_api_key"}}`)
	}))
	defer srv.Close()
	cfg := testConfig(t, srv.URL, suitePath)
	st, _ := suite.Load(suitePath, "")

	// info 级别（默认级别）：首次失败就应打印，不必开 debug
	log, buf := captureLog(slog.LevelInfo)
	out := filepath.Join(t.TempDir(), "out.rawdata.jsonl.gz")
	res, err := Run(context.Background(), cfg, st, Options{OutputPath: out, Log: log})
	if err != nil {
		t.Fatalf("全 401 不是致命错误: %v", err)
	}

	// 1. Result 带出错误分布
	if got := res.ErrorKinds["http_4xx"]; got != res.ErrorCount {
		t.Errorf("ErrorKinds[http_4xx] = %d, want %d（全部失败都是 4xx）", got, res.ErrorCount)
	}
	// 2. Result 带出样本详情（含 http_code 与端点原话）
	s, ok := res.ErrorSamples["http_4xx"]
	if !ok {
		t.Fatalf("ErrorSamples 应含 http_4xx，得到 %+v", res.ErrorSamples)
	}
	if s.HTTPCode != 401 {
		t.Errorf("样本 HTTPCode = %d, want 401", s.HTTPCode)
	}
	if !strings.Contains(s.Detail, "invalid_api_key") {
		t.Errorf("样本 Detail 应含端点原话，得到 %q", s.Detail)
	}
	// 3. 密钥不得出现在样本里（落盘与打印同一套脱敏）
	if strings.Contains(s.Detail, "sk-e2e") {
		t.Errorf("样本 Detail 泄露密钥明文: %q", s.Detail)
	}

	// 4. 首次失败在 info 级别就打印了原因
	logged := buf.String()
	if !strings.Contains(logged, "invalid_api_key") {
		t.Errorf("info 级别应打印端点错误原话，实际日志:\n%s", logged)
	}
	if !strings.Contains(logged, "http_4xx") {
		t.Errorf("应打印 error_kind:\n%s", logged)
	}
	// 5. 同类错误只打印一次，不刷屏（失败 20+ 条，WARN 行应远少于此）
	if n := strings.Count(logged, "WARN"); n > 3 {
		t.Errorf("同类错误应去重打印，WARN 行数 = %d（失败 %d 条）", n, res.ErrorCount)
	}
}

// 多种错误分类混合时，每种都应各打印一次首例。
func TestCollectSurfacesEachErrorKindOnce(t *testing.T) {
	os.Setenv("TV_E2E_KEY", "sk-e2e")
	defer os.Unsetenv("TV_E2E_KEY")
	suitePath := testSuite(t)
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 前几条 500，之后 403：制造两种 error_kind
		if n.Add(1) <= 4 {
			w.WriteHeader(500)
			fmt.Fprint(w, `{"error":{"message":"upstream exploded"}}`)
			return
		}
		w.WriteHeader(403)
		fmt.Fprint(w, `{"error":{"message":"quota exceeded"}}`)
	}))
	defer srv.Close()
	cfg := testConfig(t, srv.URL, suitePath)
	st, _ := suite.Load(suitePath, "")

	log, buf := captureLog(slog.LevelInfo)
	out := filepath.Join(t.TempDir(), "out.rawdata.jsonl.gz")
	res, err := Run(context.Background(), cfg, st, Options{OutputPath: out, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	if res.ErrorKinds["http_5xx"] == 0 || res.ErrorKinds["http_4xx"] == 0 {
		t.Errorf("应同时统计到 5xx 与 4xx，得到 %+v", res.ErrorKinds)
	}
	logged := buf.String()
	for _, want := range []string{"upstream exploded", "quota exceeded"} {
		if !strings.Contains(logged, want) {
			t.Errorf("两种错误各应打印一次首例，缺 %q:\n%s", want, logged)
		}
	}
}

// 高并发下错误账本必须自洽：分类计数之和 == 失败总数。
// 账本在多 goroutine 回调里读写，计数丢失或重复都会让这个等式不成立。
// （环境无 cgo 时 -race 跑不了，用一致性等式兜底。）
func TestCollectErrorLedgerConsistentUnderConcurrency(t *testing.T) {
	os.Setenv("TV_E2E_KEY", "sk-e2e")
	defer os.Unsetenv("TV_E2E_KEY")
	suitePath := testSuite(t)
	var n atomic.Int64
	// 轮转三种状态码，制造多分类并发写账本
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch n.Add(1) % 3 {
		case 0:
			w.WriteHeader(500)
			fmt.Fprint(w, `{"error":{"message":"boom"}}`)
		case 1:
			w.WriteHeader(429)
			fmt.Fprint(w, `{"error":{"message":"slow down"}}`)
		default:
			w.WriteHeader(404)
			fmt.Fprint(w, `{"error":{"message":"no such model"}}`)
		}
	}))
	defer srv.Close()
	cfg := testConfig(t, srv.URL, suitePath)
	cfg.Runtime.MaxConcurrency = 16 // 压并发
	st, _ := suite.Load(suitePath, "")

	out := filepath.Join(t.TempDir(), "out.rawdata.jsonl.gz")
	res, err := Run(context.Background(), cfg, st, Options{OutputPath: out})
	if err != nil {
		t.Fatal(err)
	}
	sum := 0
	for _, c := range res.ErrorKinds {
		sum += c
	}
	if sum != res.ErrorCount {
		t.Errorf("账本计数之和 %d != ErrorCount %d（并发下有丢失或重复）:\n  %+v",
			sum, res.ErrorCount, res.ErrorKinds)
	}
	// 每个出现过的分类都应有且仅有一个首例样本
	if len(res.ErrorSamples) != len(res.ErrorKinds) {
		t.Errorf("样本数 %d 与分类数 %d 不符: samples=%+v kinds=%+v",
			len(res.ErrorSamples), len(res.ErrorKinds), res.ErrorSamples, res.ErrorKinds)
	}
}

// 端点返回的 error_detail 含 ANSI 转义时，打印到终端前必须已被剥掉。
func TestCollectSanitizesErrorDetailForTerminal(t *testing.T) {
	os.Setenv("TV_E2E_KEY", "sk-e2e")
	defer os.Unsetenv("TV_E2E_KEY")
	suitePath := testSuite(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		// 恶意端点：用 ESC 清屏 + CR 覆盖已打印内容伪造终端显示
		fmt.Fprint(w, "{\"error\":{\"message\":\"\x1b[2J\x1b[H\rFAKE: collect 完成\"}}")
	}))
	defer srv.Close()
	cfg := testConfig(t, srv.URL, suitePath)
	st, _ := suite.Load(suitePath, "")

	log, buf := captureLog(slog.LevelInfo)
	out := filepath.Join(t.TempDir(), "out.rawdata.jsonl.gz")
	if _, err := Run(context.Background(), cfg, st, Options{OutputPath: out, Log: log}); err != nil {
		t.Fatal(err)
	}
	logged := buf.String()
	if strings.ContainsAny(logged, "\x1b\r") {
		t.Errorf("打印到终端的错误详情残留 ESC/CR: %q", logged)
	}
	// 落盘的证据仍应是原始字节（脱敏除外）：终端安全在渲染层解决，不改证据
	f, err := rawdata.Read(out)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, r := range f.Records {
		if r.ErrorDetail != nil && strings.Contains(*r.ErrorDetail, "\x1b") {
			found = true
			break
		}
	}
	if !found {
		t.Error("落盘的 error_detail 应保留原始控制字符（证据不被渲染层改写）")
	}
}

// captureEndpoint 记录每条请求的 (system, user) 消息，返回契约 JSON 格式的答案。
type captureEndpoint struct {
	mu    sync.Mutex
	calls []capturedCall
}

type capturedCall struct {
	hasSystem bool
	system    string
	user      string
}

func (c *captureEndpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &req)
	var call capturedCall
	for _, m := range req.Messages {
		if m.Role == "system" {
			call.hasSystem = true
			call.system = m.Content
		}
		if m.Role == "user" {
			call.user = m.Content
		}
	}
	c.mu.Lock()
	c.calls = append(c.calls, call)
	c.mu.Unlock()

	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"{\\\"answer\\\": \\\"42\\\"}\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":5}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		return
	}
	w.WriteHeader(200)
	fmt.Fprint(w, `{"choices":[{"message":{"content":"{\"answer\": \"42\"}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":5}}`)
}

// contractSuite 带输出契约的小题库：onetoken（normalize=number）应命中契约，
// tokenizer（normalize=none）应豁免。
func contractSuite(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "suite.yaml")
	content := `suite_version: v2
output_contract:
  field: answer
  system_prompt: '请只回答 JSON {"answer": "<your answer>"}'
probes:
  onetoken:
    normalize: number
    items:
      - id: ot.v2.001
        prompt: "说一个数"
  tokenizer:
    normalize: none
    items:
      - id: tk.v2.001
        prompt: "重复：こんにちは"
`
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCollectContractSystemPrompt(t *testing.T) {
	os.Setenv("TV_E2E_KEY", "sk-e2e")
	defer os.Unsetenv("TV_E2E_KEY")

	srv := httptest.NewServer(&captureEndpoint{})
	defer srv.Close()
	ep := srv.Config.Handler.(*captureEndpoint)

	suitePath := contractSuite(t)
	cfg := testConfig(t, srv.URL, suitePath)
	cfg.Probes["onetoken"] = config.ProbeConfig{Enabled: true, Repeats: 2,
		Temperature: f64p(1.0), MaxTokens: intptr(64), ContextBuckets: []int{0}}
	cfg.Probes["tokenizer"] = config.ProbeConfig{Enabled: true, Repeats: 1,
		Temperature: f64p(0.0), MaxTokens: intptr(64), ContextBuckets: []int{0}}
	st, err := suite.Load(suitePath, "")
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "contract.rawdata.jsonl.gz")
	res, err := Run(context.Background(), cfg, st, Options{OutputPath: out})
	if err != nil {
		t.Fatal(err)
	}
	if res.ErrorCount > 0 {
		t.Fatalf("res = %+v", res)
	}

	ep.mu.Lock()
	defer ep.mu.Unlock()
	var otCalls, tkCalls int
	for _, c := range ep.calls {
		if contains(c.user, "重复") { // tokenizer 题面以「重复」开头
			tkCalls++
			if c.hasSystem {
				t.Errorf("tokenizer（normalize=none）不应发 system prompt: %+v", c)
			}
		} else {
			otCalls++
			if !c.hasSystem {
				t.Errorf("onetoken（normalize=number）应发 system prompt: %+v", c)
			}
			if c.system != `请只回答 JSON {"answer": "<your answer>"}` {
				t.Errorf("system = %q", c.system)
			}
		}
	}
	if otCalls != 2 || tkCalls != 1 {
		t.Errorf("调用数 ot=%d tk=%d, want 2/1", otCalls, tkCalls)
	}

	// 观测端：模型回的契约 JSON {"answer": "42"} 应被解包归一化为 42
	recs := readRecords(t, out)
	var sawOT bool
	for _, r := range recs {
		if r.ProbeID == "onetoken" && r.Status == "success" {
			sawOT = true
			var obs map[string]string
			if err := json.Unmarshal(r.Observation, &obs); err != nil {
				t.Fatalf("observation: %v", err)
			}
			if obs["value"] != "42" {
				t.Errorf("onetoken value = %q, want 42（契约 JSON 解包）", obs["value"])
			}
		}
	}
	if !sawOT {
		t.Error("没有采到 onetoken 记录")
	}
}

func contains(s, sub string) bool { return bytes.Contains([]byte(s), []byte(sub)) }

// readRecords 解压读回全部记录。
func readRecords(t *testing.T, path string) []rawdata.Record {
	t.Helper()
	f, err := rawdata.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	return f.Records
}

func TestManifestBaseURLFingerprint(t *testing.T) {
	os.Setenv("TV_E2E_KEY", "sk-e2e")
	defer os.Unsetenv("TV_E2E_KEY")
	suitePath := testSuite(t)
	srv := httptest.NewServer(fakeOpenAI{mode: "same"})
	defer srv.Close()
	cfg := testConfig(t, srv.URL, suitePath)
	// 只保留一个档位一题，缩短采集
	cfg.Probes["tokenizer"] = config.ProbeConfig{}
	st, _ := suite.Load(suitePath, "")

	out := filepath.Join(t.TempDir(), "out.rawdata.jsonl.gz")
	if _, err := Run(context.Background(), cfg, st, Options{OutputPath: out}); err != nil {
		t.Fatal(err)
	}
	f, err := rawdata.Read(out)
	if err != nil {
		t.Fatal(err)
	}

	fp := f.Manifest.Endpoint.BaseURLFingerprint
	if len(fp) != 16 {
		t.Errorf("base_url_fingerprint = %q, want 16 hex chars", fp)
	}
	for _, c := range fp {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			t.Errorf("base_url_fingerprint 应为小写 hex: %q", fp)
			break
		}
	}
	// manifest 中不出现明文主机名（host:port）
	raw, _ := json.Marshal(f.Manifest)
	if bytes.Contains(raw, []byte("127.0.0.1")) {
		t.Error("manifest 中出现明文 base_url host")
	}

	// 同 URL → 同指纹；不同 URL → 不同指纹（确定性 + 可区分换端点）
	cfg2 := testConfig(t, srv.URL, suitePath)
	cfg2.Probes["tokenizer"] = config.ProbeConfig{}
	out2 := filepath.Join(t.TempDir(), "out2.rawdata.jsonl.gz")
	if _, err := Run(context.Background(), cfg2, st, Options{OutputPath: out2}); err != nil {
		t.Fatal(err)
	}
	f2, _ := rawdata.Read(out2)
	if f2.Manifest.Endpoint.BaseURLFingerprint != fp {
		t.Error("同一 base_url 两次采集指纹应相同")
	}

	srv2 := httptest.NewServer(fakeOpenAI{mode: "same"})
	defer srv2.Close()
	cfg3 := testConfig(t, srv2.URL, suitePath)
	cfg3.Probes["tokenizer"] = config.ProbeConfig{}
	out3 := filepath.Join(t.TempDir(), "out3.rawdata.jsonl.gz")
	if _, err := Run(context.Background(), cfg3, st, Options{OutputPath: out3}); err != nil {
		t.Fatal(err)
	}
	f3, _ := rawdata.Read(out3)
	if f3.Manifest.Endpoint.BaseURLFingerprint == fp {
		t.Error("不同 base_url 指纹应不同")
	}
}
