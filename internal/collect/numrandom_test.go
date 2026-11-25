package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/chaterm/token-verifier/internal/adapter"
	"github.com/chaterm/token-verifier/internal/config"
	"github.com/chaterm/token-verifier/internal/rawdata"
	"github.com/chaterm/token-verifier/internal/suite"
)

func buildIntCSV(n int) string {
	parts := make([]string, n)
	for i := 0; i < n; i++ {
		parts[i] = fmt.Sprintf("%d", 1+(i*37)%355)
	}
	return strings.Join(parts, ", ")
}

func TestObserveNumRandomParsingAndCoverage(t *testing.T) {
	seq200 := buildIntCSV(200)

	// 1. 标准 JSON 数组
	raw, err := ObserveNumrandom(adapter.Response{Content: "[" + seq200 + "]"}, 1, 355, 300)
	if err != nil {
		t.Fatalf("标准 JSON 数组应解析成功: %v", err)
	}
	var parsed struct {
		Values      []int `json:"values"`
		MinValue    int   `json:"min_value"`
		MaxValue    int   `json:"max_value"`
		TargetCount int   `json:"target_count"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Values) != 200 || parsed.MinValue != 1 || parsed.MaxValue != 355 || parsed.TargetCount != 300 {
		t.Fatalf("观测结构不符: len=%d, meta=%+v", len(parsed.Values), parsed)
	}

	// 2. Markdown 围栏 + 前后说明文字 + 混入越界值/负数/小数/全角数字：
	//    字母切断前后说明，游程内部越界值(0, 356)、负数(-12)、小数(3.14)、全角(１２)均不计入合法序列
	noisy := "Here are 300 numbers:\n```json\n[0, -12, 3.14, １２, " + seq200 + ", 356]\n```\nDone with step 2."
	raw2, err := ObserveNumrandom(adapter.Response{Content: noisy}, 1, 355, 300)
	if err != nil {
		t.Fatalf("围栏与非合法数值过滤后应成功: %v", err)
	}
	var parsed2 struct {
		Values []int `json:"values"`
	}
	_ = json.Unmarshal(raw2, &parsed2)
	if len(parsed2.Values) != 200 {
		t.Fatalf("过滤噪声后期望 200 个有效整数, 实际 %d", len(parsed2.Values))
	}

	// 3. 截断门控：target=300 时门槛为 max(80, ceil(0.55*300)) = 165；164 应拒收，165 应通过
	if _, err := ObserveNumrandom(adapter.Response{Content: "[" + buildIntCSV(164) + "]"}, 1, 355, 300); err == nil {
		t.Error("164 < 165 应因有效整数不足报错")
	}
	if _, err := ObserveNumrandom(adapter.Response{Content: "[" + buildIntCSV(165) + "]"}, 1, 355, 300); err != nil {
		t.Errorf("165 == 门槛应通过, got err: %v", err)
	}
}

func TestObserveNumRandomTruncatesToTargetCount(t *testing.T) {
	// 模型未按 target_count 停止、持续失控生成（线上实测单响应可写出 6 万+整数）：
	// 观测必须只保留前 target_count 个整数，且保留顺序与原始前缀一致。
	seq := buildIntCSV(600)
	raw, err := ObserveNumrandom(adapter.Response{Content: "[" + seq + "]"}, 1, 355, 292)
	if err != nil {
		t.Fatalf("超长序列应被截断而不是报错: %v", err)
	}
	var parsed struct {
		Values []int `json:"values"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Values) != 292 {
		t.Fatalf("len(values) = %d, want 292（应按 target_count 截断）", len(parsed.Values))
	}
	// 校验前缀逐位一致（buildIntCSV 为确定性序列，取前 292 个）
	for i := 0; i < 292; i++ {
		expected := 1 + (i*37)%355
		if parsed.Values[i] != expected {
			t.Fatalf("values[%d] = %d, want %d（截断应保留原始前缀顺序）", i, parsed.Values[i], expected)
		}
	}
}

func TestCollectNumRandomNoSystemContractAndNullTemp(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		mu.Lock()
		bodies = append(bodies, m)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"content":"[%s]"}}],"usage":{"prompt_tokens":30,"completion_tokens":400}}`,
			buildIntCSV(292))
	}))
	defer srv.Close()

	st, err := suite.Parse([]byte(`suite_version: v1
output_contract:
  field: answer
  system_prompt: '请只回答 JSON 对象'
probes:
  numrandom:
    normalize: none
    thinking_effort: null
    min_value: 1
    max_value: 355
    items:
      - id: nr.v1.001
        target_count: 292
        prompt: "Output 292 integers in [1,355]"
`), "")
	if err != nil {
		t.Fatal(err)
	}

	mt := 4096
	cfg := &config.File{
		Version: 1,
		Target: config.Target{
			BaseURL:   srv.URL,
			APIKeyEnv: "TV_TEST_NR_KEY",
			Model:     "test-model",
			Protocols: []config.ProtocolConfig{{
				ID:           "openai-chat",
				Path:         "/v1/chat/completions",
				Weight:       1,
				Capabilities: config.Capabilities{Stream: true, Tools: true, Thinking: true, Temperature: true, ContextWindow: 128000},
				Transport:    config.TransportMix{NonStream: 1.0},
			}},
			Request: config.RequestConfig{TemperatureField: "temperature"},
		},
		Probes: map[string]config.ProbeConfig{
			"numrandom": {
				Enabled:        true,
				Repeats:        2,
				Temperature:    nil, // 锁温模型省略
				MaxTokens:      &mt,
				ContextBuckets: []int{0},
			},
		},
		Runtime: config.Runtime{
			MaxConcurrency: 2,
			TimeoutSec:     10,
			MaxAttempts:    1,
			Seed:           42,
			RawLevel:       "digest",
			BackoffBaseMs:  100,
			BackoffMaxMs:   1000,
		},
	}
	t.Setenv("TV_TEST_NR_KEY", "sk-test")

	outPath := filepath.Join(t.TempDir(), "nr.rawdata.jsonl.gz")
	if _, err := Run(context.Background(), cfg, st, Options{OutputPath: outPath, APIKey: "sk-test"}); err != nil {
		t.Fatalf("Collect Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("请求数 = %d, want 2", len(bodies))
	}
	for i, body := range bodies {
		if _, hasTemp := body["temperature"]; hasTemp {
			t.Errorf("req %d: temperature=nil 时请求体不应带 temperature: %+v", i, body)
		}
		msgs, _ := body["messages"].([]any)
		for _, rawMsg := range msgs {
			msg, _ := rawMsg.(map[string]any)
			if msg["role"] == "system" {
				t.Errorf("req %d: numrandom (normalize:none) 绝不应注入 output_contract system prompt: %+v", i, msg)
			}
		}
	}

	rf, err := rawdata.Read(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(rf.Records) != 2 || rf.Records[0].Status != "success" {
		t.Fatalf("落盘记录异常: %+v", rf.Records)
	}
}
