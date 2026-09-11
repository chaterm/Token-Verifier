package collect

// 三新探针（needle / think-effort / toolcall）的端到端采集测试：
// 假端点按探针语义回响应 → collect.Run 产出 rawData → observation 形状验证。
// 不打真实 API。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/chaterm/token-verifier/internal/compare"
	"github.com/chaterm/token-verifier/internal/config"
	"github.com/chaterm/token-verifier/internal/rawdata"
	"github.com/chaterm/token-verifier/internal/suite"
)

// fakeProbeEP 按请求体特征回探针语义的响应：
//   - 请求体含 tools → 回 tool_calls（degraded=true 时一半回非法参数）
//   - prompt 里能提取 NEEDLE- 标记 → 全部复述（degraded=true 时只复述第一个）
//   - 其余 → 回带 reasoning_tokens 的 usage（degraded=true 时思考数砍到 1/10）
type fakeProbeEP struct {
	degraded bool
}

var needleMarkerRe = regexp.MustCompile(`NEEDLE-[A-Z2-9]{8}`)

func (f fakeProbeEP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Tools []map[string]any `json:"tools"`
	}
	_ = json.Unmarshal(body, &req)

	prompt := ""
	for _, m := range req.Messages {
		if m.Role == "user" {
			prompt = m.Content
			break
		}
	}

	markers := needleMarkerRe.FindAllString(prompt, -1)
	w.Header().Set("Content-Type", "application/json")
	switch {
	case len(req.Tools) > 0:
		// toolcall：回首个工具的调用
		toolName := ""
		if fn, ok := req.Tools[0]["function"].(map[string]any); ok {
			toolName, _ = fn["name"].(string)
		}
		args := `{"city":"北京"}`
		if f.degraded {
			args = `{"city": 123, }` // 非法 JSON → args_valid=false
		}
		fmt.Fprintf(w, `{"choices":[{"message":{"content":"","tool_calls":[{"function":{"name":%q,"arguments":%s}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":30,"completion_tokens":8}}`,
			toolName, strconv.Quote(args))
	case len(markers) > 0:
		// needle：把 prompt 里出现的标记逐行复述（模拟完美召回）
		if f.degraded && len(markers) > 1 {
			markers = markers[:1] // 截断的端点：只看到第一个埋点
		}
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":5}}`,
			strings.Join(markers, "\n"))
	default:
		// think-effort：回思考 token 数
		reasoning := 800
		if f.degraded {
			reasoning = 80 // 预算被削减
		}
		fmt.Fprintf(w, `{"choices":[{"message":{"content":"对","reasoning_content":"思考过程"},"finish_reason":"stop"}],"usage":{"prompt_tokens":40,"completion_tokens":100,"completion_tokens_details":{"reasoning_tokens":%d}}}`, reasoning)
	}
}

// probeSuite 三探针的小题库（needle 单题双埋点、think-effort 双档、toolcall 单题）。
func probeSuite(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "suite.yaml")
	content := `suite_version: t3
probes:
  needle:
    normalize: none
    items:
      - id: nd.t.001
        prompt: "请按出现顺序逐行输出你找到的全部标记。"
        needle_positions: [0.3, 0.7]
  think-effort:
    normalize: none
    items:
      - id: te.t.001
        prompt: "估算 17 乘 23，先思考再给出结果。"
        thinking_effort: low
      - id: te.t.002
        prompt: "比较两种排序算法，先思考再回答。"
        thinking_effort: high
  toolcall:
    normalize: none
    tools:
      - name: get_weather
        description: 获取某城市当前天气
        parameters:
          type: object
          properties:
            city: { type: string }
          required: [city]
    items:
      - id: tc.t.001
        prompt: "北京现在多少度？"
`
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// probeConfig 三探针的采集配置（bucket 用小值，填充文本短，测试快）。
func probeConfig(t *testing.T, baseURL, suitePath string) *config.File {
	t.Helper()
	f := &config.File{
		Version: 1,
		Target: config.Target{
			BaseURL:   baseURL,
			APIKeyEnv: "TV_E2E_KEY",
			Model:     "fake-model",
			Protocols: []config.ProtocolConfig{
				{ID: "openai-chat", Weight: 1.0,
					Transport:    config.TransportMix{Stream: 0.0, NonStream: 1.0},
					Capabilities: config.Capabilities{Stream: true, Tools: true, Thinking: true},
					Thinking: config.ThinkingBlock{
						Disabled: config.ThinkingMode{BodyOverrides: map[string]any{}},
						Enabled: config.ThinkingMode{
							EffortMap:     map[string]any{"low": "low", "high": "high"},
							BodyOverrides: map[string]any{"reasoning_effort": "${thinking_effort}"},
						},
					}},
			},
			Request: config.RequestConfig{TemperatureField: "temperature"},
		},
		Suite: config.SuiteRef{Path: suitePath},
		Probes: map[string]config.ProbeConfig{
			"needle":       {Enabled: true, Repeats: 2, Temperature: f64p(0.0), MaxTokens: intptr(64), ContextBuckets: []int{100}},
			"think-effort": {Enabled: true, Repeats: 6, Temperature: f64p(1.0), MaxTokens: intptr(2048), ContextBuckets: []int{0}},
			"toolcall":     {Enabled: true, Repeats: 6, Temperature: f64p(1.0), MaxTokens: intptr(512), ContextBuckets: []int{0}},
		},
		Runtime: config.Runtime{MaxConcurrency: 2, TimeoutSec: 10, MaxAttempts: 2, Seed: 7, RawLevel: "digest"},
	}
	f.ApplyDefaults()
	return f
}

func collectProbes(t *testing.T, degraded bool) string {
	t.Helper()
	os.Setenv("TV_E2E_KEY", "sk-e2e")
	defer os.Unsetenv("TV_E2E_KEY")
	srv := httptest.NewServer(fakeProbeEP{degraded: degraded})
	defer srv.Close()

	suitePath := probeSuite(t)
	st, err := suite.Load(suitePath, "")
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "out.rawdata.jsonl.gz")
	res, err := Run(context.Background(), probeConfig(t, srv.URL, suitePath), st, Options{OutputPath: out})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if res.ErrorCount > 0 {
		t.Fatalf("res = %+v，不应有错误请求", res)
	}
	return out
}

func TestNewProbesObservations(t *testing.T) {
	out := collectProbes(t, false)
	f, err := rawdata.Read(out)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, rec := range f.Records {
		if rec.Status != "success" {
			t.Errorf("record %s/%s status = %s (%v)", rec.ProbeID, rec.QuestionID, rec.Status, rec.ErrorDetail)
			continue
		}
		seen[rec.ProbeID]++
		switch rec.ProbeID {
		case "needle":
			var o struct {
				Matched []bool `json:"matched"`
			}
			if err := json.Unmarshal(rec.Observation, &o); err != nil {
				t.Fatalf("needle observation %s: %v", rec.Observation, err)
			}
			if len(o.Matched) != 2 || !o.Matched[0] || !o.Matched[1] {
				t.Errorf("needle observation = %v, want [true true]（完美召回）", o.Matched)
			}
			// thinking_effort 应为 null（题目未标注）
			if rec.ThinkingEffort != nil {
				t.Errorf("needle record thinking_effort = %v, want nil", *rec.ThinkingEffort)
			}
		case "think-effort":
			var o struct {
				ReasoningValue int    `json:"reasoning_value"`
				Unit           string `json:"unit"`
			}
			if err := json.Unmarshal(rec.Observation, &o); err != nil {
				t.Fatalf("think-effort observation %s: %v", rec.Observation, err)
			}
			if o.ReasoningValue != 800 {
				t.Errorf("reasoning_value = %d, want 800", o.ReasoningValue)
			}
			if o.Unit != "tokens" {
				t.Errorf("unit = %s, want tokens（openai-chat 协议）", o.Unit)
			}
			if rec.ThinkingEffort == nil {
				t.Errorf("think-effort record 必须带强度级别: %+v", rec)
			}
		case "toolcall":
			var o struct {
				Tool      string `json:"tool"`
				ArgsValid bool   `json:"args_valid"`
				Parallel  bool   `json:"parallel"`
				NumCalls  int    `json:"num_calls"`
			}
			if err := json.Unmarshal(rec.Observation, &o); err != nil {
				t.Fatalf("toolcall observation %s: %v", rec.Observation, err)
			}
			if o.Tool != "get_weather" || !o.ArgsValid || o.Parallel || o.NumCalls != 1 {
				t.Errorf("toolcall observation = %+v", o)
			}
		}
	}
	for _, pid := range []string{"needle", "think-effort", "toolcall"} {
		if seen[pid] == 0 {
			t.Errorf("探针 %s 没有成功 record", pid)
		}
	}
}

func TestNewProbesNeedlePaddingBuriesMarkers(t *testing.T) {
	// 埋点真的进了请求体：端点收到的 user 消息应含 2 个派生标记
	var mu sync.Mutex
	var gotPrompt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		mu.Lock()
		for _, m := range req.Messages {
			if m.Role == "user" {
				gotPrompt = m.Content
			}
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"x"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":1}}`)
	}))
	defer srv.Close()

	os.Setenv("TV_E2E_KEY", "sk-e2e")
	defer os.Unsetenv("TV_E2E_KEY")
	suitePath := probeSuite(t)
	st, _ := suite.Load(suitePath, "")
	// 只跑 needle：其余探针关掉
	cfg := probeConfig(t, srv.URL, suitePath)
	delete(cfg.Probes, "think-effort")
	delete(cfg.Probes, "toolcall")
	out := filepath.Join(t.TempDir(), "out.rawdata.jsonl.gz")
	if _, err := Run(context.Background(), cfg, st, Options{OutputPath: out}); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	prompt := gotPrompt
	mu.Unlock()
	markers := suite.NeedleMarkers("nd.t.001", []float64{0.3, 0.7})
	for i, m := range markers {
		if !strings.Contains(prompt, m) {
			t.Errorf("请求体缺埋点标记[%d] %s", i, m)
		}
	}
	// 填充语料也在（bucket=100 → 约 75 词）
	if len(prompt) < 200 {
		t.Errorf("请求体太短，填充语料疑似缺失: len=%d", len(prompt))
	}
	// 题面在最后
	if !strings.HasSuffix(strings.TrimSpace(prompt), "请按出现顺序逐行输出你找到的全部标记。") {
		t.Errorf("题面应在语料之后: tail=%q", prompt[max(0, len(prompt)-60):])
	}
}

func TestNewProbesDegradedEndpointCompare(t *testing.T) {
	// 正常端点 vs 降级端点（needle 截断、思考预算削减、参数非法）：
	// think-effort 与 toolcall 应判 fail，needle 单元数不足以稳定判 fail
	// 但观测差异必须体现在 cell 明细里。
	outA := collectProbes(t, false)
	outB := collectProbes(t, true)

	fileA, err := rawdata.Read(outA)
	if err != nil {
		t.Fatal(err)
	}
	fileB, err := rawdata.Read(outB)
	if err != nil {
		t.Fatal(err)
	}
	thresholds := map[string]float64{"needle": 0.01, "think-effort": 0.01, "toolcall": 0.05}
	res, err := compare.Run(fileA, fileB, thresholds, nil)
	if err != nil {
		t.Fatal(err)
	}
	byProbe := map[string]string{}
	for _, v := range res.Verdicts {
		byProbe[v.ProbeID] = v.Verdict
	}
	if byProbe["think-effort"] != "fail" {
		t.Errorf("思考预算削减应判 fail: %s", byProbe["think-effort"])
	}
	if byProbe["toolcall"] != "fail" {
		t.Errorf("参数合法率崩塌应判 fail: %s", byProbe["toolcall"])
	}
}

func TestThinkEffortAnthropicCharsUnit(t *testing.T) {
	// anthropic 协议端到端：usage 无 reasoning 字段，但 thinking 块交付 →
	// 观测应为 chars 单位、值 = 思考文本 rune 数（不再是观测错误）
	thinking := "先枚举前提，再逐步推理，最后得出结论。"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"content":[{"type":"thinking","thinking":%s},{"type":"text","text":"错"}],"usage":{"input_tokens":20,"output_tokens":50}}`,
			strconv.Quote(thinking))
	}))
	defer srv.Close()

	os.Setenv("TV_E2E_KEY", "sk-e2e")
	defer os.Unsetenv("TV_E2E_KEY")
	suitePath := probeSuite(t)
	st, err := suite.Load(suitePath, "")
	if err != nil {
		t.Fatal(err)
	}
	cfg := probeConfig(t, srv.URL, suitePath)
	cfg.Target.Protocols = []config.ProtocolConfig{
		{ID: "anthropic-messages", Weight: 1.0,
			Transport:    config.TransportMix{Stream: 0.0, NonStream: 1.0},
			Capabilities: config.Capabilities{Stream: true, Tools: true, Thinking: true},
			Thinking: config.ThinkingBlock{
				Disabled: config.ThinkingMode{BodyOverrides: map[string]any{}},
				Enabled: config.ThinkingMode{
					EffortMap:     map[string]any{"low": "low", "high": "high"},
					BodyOverrides: map[string]any{"output_config": map[string]any{"effort": "${thinking_effort}"}},
				},
			}},
	}
	// 只跑 think-effort
	delete(cfg.Probes, "needle")
	delete(cfg.Probes, "toolcall")
	cfg.Probes["think-effort"] = config.ProbeConfig{Enabled: true, Repeats: 2, Temperature: f64p(1.0), MaxTokens: intptr(2048), ContextBuckets: []int{0}}

	out := filepath.Join(t.TempDir(), "out.rawdata.jsonl.gz")
	res, err := Run(context.Background(), cfg, st, Options{OutputPath: out})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if res.ErrorCount > 0 {
		t.Fatalf("anthropic 协议下 think-effort 不应再有观测错误: %+v", res)
	}
	f, err := rawdata.Read(out)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, rec := range f.Records {
		if rec.ProbeID != "think-effort" || rec.Status != "success" {
			continue
		}
		n++
		var o struct {
			ReasoningValue int    `json:"reasoning_value"`
			Unit           string `json:"unit"`
		}
		if err := json.Unmarshal(rec.Observation, &o); err != nil {
			t.Fatalf("observation %s: %v", rec.Observation, err)
		}
		if o.Unit != "chars" {
			t.Errorf("unit = %s, want chars", o.Unit)
		}
		if o.ReasoningValue != utf8.RuneCountInString(thinking) {
			t.Errorf("reasoning_value = %d, want %d（rune 数）", o.ReasoningValue, utf8.RuneCountInString(thinking))
		}
		if rec.Protocol != "anthropic-messages" {
			t.Errorf("protocol = %s", rec.Protocol)
		}
	}
	if n == 0 {
		t.Error("没有 think-effort 成功 record")
	}
}
