package collect

import (
	"encoding/json"
	"testing"

	"github.com/chaterm/token-verifier/internal/adapter"
	"github.com/chaterm/token-verifier/internal/suite"
)

// testNormalizer 空归一化空间的执行器（number/letter/word 是机理规则，不需要词表）。
func testNormalizer() *suite.Normalizer {
	f := &suite.File{SuiteVersion: "t"}
	return f.Normalizer()
}

func obsValue(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("observation 不是 {value}: %v", err)
	}
	return m["value"]
}

func TestObserveOnetokenWithJSONContract(t *testing.T) {
	norm := testNormalizer()
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"契约 JSON", `{"answer": "42"}`, "42"},
		{"数字值", `{"answer": 7}`, "7"},
		{"fence 包裹", "```json\n{\"answer\": \"13\"}\n```", "13"},
		{"嵌入文本", `答案是 {"answer": "99"}。`, "99"},
		// 契约失效（模型没守 JSON）→ 回退原文归一化，仍提取到数字
		{"回退原文", "我猜是 55 吧", "55"},
	}
	for _, c := range cases {
		resp := adapter.Response{Content: c.content}
		obs, err := ObserveOnetoken(resp, norm, "number", "answer")
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := obsValue(t, obs); got != c.want {
			t.Errorf("%s: value = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestObserveOnetokenNoContractUnchanged(t *testing.T) {
	// jsonField 为空（raw 契约）：行为与既有实现完全一致，不做 JSON 解包
	norm := testNormalizer()
	resp := adapter.Response{Content: "  42  "}
	obs, err := ObserveOnetoken(resp, norm, "number", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := obsValue(t, obs); got != "42" {
		t.Errorf("value = %q, want 42", got)
	}
}

func TestObserveOnetokenEmptyValueError(t *testing.T) {
	norm := testNormalizer()
	_, err := ObserveOnetoken(adapter.Response{Content: "没有任何数字"}, norm, "number", "answer")
	if err == nil {
		t.Fatal("归一化为空应报错")
	}
}

func TestObserveTokenizerUnchanged(t *testing.T) {
	obs, err := ObserveTokenizer(adapter.Response{Usage: adapter.Usage{Prompt: 128}})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]int
	if err := json.Unmarshal(obs, &m); err != nil {
		t.Fatal(err)
	}
	if m["prompt_tokens"] != 128 {
		t.Errorf("prompt_tokens = %d", m["prompt_tokens"])
	}
	if _, err := ObserveTokenizer(adapter.Response{}); err == nil {
		t.Error("usage 缺失应报错")
	}
}

func TestObserveNeedle(t *testing.T) {
	markers := []string{"NEEDLE-ABC23456", "NEEDLE-XYZ78923"}
	cases := []struct {
		name string
		body string
		want []bool
	}{
		{"全中", "我找到 NEEDLE-ABC23456 和 needle-xyz78923。", []bool{true, true}},
		{"部分中", "NEEDLE-ABC23456", []bool{true, false}},
		{"全漏", "没有找到任何标记", []bool{false, false}},
		// 标记嵌在整句引用里也算命中（大小写不敏感 + 子串包含）
		{"整句引用", "读到该标记请原样输出：needle-abc23456", []bool{true, false}},
	}
	for _, c := range cases {
		obs, err := ObserveNeedle(adapter.Response{Content: c.body}, markers)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		var m struct {
			Matched []bool `json:"matched"`
		}
		if err := json.Unmarshal(obs, &m); err != nil {
			t.Fatalf("%s: observation 不是 {matched: [...]}: %v", c.name, err)
		}
		if len(m.Matched) != len(c.want) {
			t.Fatalf("%s: matched 长度 = %d, want %d", c.name, len(m.Matched), len(c.want))
		}
		for i := range c.want {
			if m.Matched[i] != c.want[i] {
				t.Errorf("%s: matched[%d] = %v, want %v", c.name, i, m.Matched[i], c.want[i])
			}
		}
	}
	// 无标记 → 错误
	if _, err := ObserveNeedle(adapter.Response{Content: "x"}, nil); err == nil {
		t.Error("markers 为空应报错")
	}
}

func TestObserveThinkEffort(t *testing.T) {
	obs, err := ObserveThinkEffort(adapter.Response{Usage: adapter.Usage{Reasoning: 512}}, "tokens")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(obs, &m); err != nil {
		t.Fatal(err)
	}
	if m["reasoning_value"] != float64(512) {
		t.Errorf("reasoning_value = %v, want 512", m["reasoning_value"])
	}
	if m["unit"] != "tokens" {
		t.Errorf("unit = %v, want tokens", m["unit"])
	}
	// 缺失/为零 → 观测错误（区分「没上报」与「零思考」）
	if _, err := ObserveThinkEffort(adapter.Response{}, "tokens"); err == nil {
		t.Error("usage.reasoning 缺失应报错")
	}
	// chars 单位：数思考文本 rune 数；空思考 = 0 是有效观测（降级信号）
	obs2, err := ObserveThinkEffort(adapter.Response{ReasoningContent: "思考了五个字"}, "chars")
	if err != nil {
		t.Fatal(err)
	}
	var m2 map[string]any
	if err := json.Unmarshal(obs2, &m2); err != nil {
		t.Fatal(err)
	}
	if m2["reasoning_value"] != float64(6) {
		t.Errorf("reasoning_value = %v, want 6 (rune 数)", m2["reasoning_value"])
	}
	if m2["unit"] != "chars" {
		t.Errorf("unit = %v, want chars", m2["unit"])
	}
	obs3, err := ObserveThinkEffort(adapter.Response{Usage: adapter.Usage{Reasoning: 999}}, "chars")
	if err != nil {
		t.Fatal(err)
	}
	var m3 map[string]any
	if err := json.Unmarshal(obs3, &m3); err != nil {
		t.Fatal(err)
	}
	if m3["reasoning_value"] != float64(0) {
		t.Errorf("chars 单位下 usage.reasoning 不应被使用: %v", m3["reasoning_value"])
	}
}

func TestObserveToolCall(t *testing.T) {
	tools := []string{"get_weather", "get_forecast"}
	cases := []struct {
		name      string
		resp      adapter.Response
		wantTool  string
		wantValid bool
		wantPar   bool
		wantCalls int
	}{
		{
			name: "单调用合法",
			resp: adapter.Response{ToolCalls: []adapter.ToolCall{
				{Name: "get_weather", Args: json.RawMessage(`{"city":"北京"}`)},
			}},
			wantTool: "get_weather", wantValid: true, wantPar: false, wantCalls: 1,
		},
		{
			name: "并行调用",
			resp: adapter.Response{ToolCalls: []adapter.ToolCall{
				{Name: "get_weather", Args: json.RawMessage(`{"city":"上海"}`)},
				{Name: "get_weather", Args: json.RawMessage(`{"city":"广州"}`)},
			}},
			wantTool: "get_weather", wantValid: true, wantPar: true, wantCalls: 2,
		},
		{
			name: "未知工具名",
			resp: adapter.Response{ToolCalls: []adapter.ToolCall{
				{Name: "hallucinated_tool", Args: json.RawMessage(`{"x":1}`)},
			}},
			wantTool: "hallucinated_tool", wantValid: false, wantPar: false, wantCalls: 1,
		},
		{
			name: "参数非法 JSON",
			resp: adapter.Response{ToolCalls: []adapter.ToolCall{
				{Name: "get_weather", Args: json.RawMessage(`{"city":`)},
			}},
			wantTool: "get_weather", wantValid: false, wantPar: false, wantCalls: 1,
		},
		{
			name: "参数不是 object",
			resp: adapter.Response{ToolCalls: []adapter.ToolCall{
				{Name: "get_weather", Args: json.RawMessage(`"北京"`)},
			}},
			wantTool: "get_weather", wantValid: false, wantPar: false, wantCalls: 1,
		},
	}
	for _, c := range cases {
		obs, err := ObserveToolCall(c.resp, tools)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		var m struct {
			Tool      string `json:"tool"`
			ArgsValid bool   `json:"args_valid"`
			Parallel  bool   `json:"parallel"`
			NumCalls  int    `json:"num_calls"`
		}
		if err := json.Unmarshal(obs, &m); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if m.Tool != c.wantTool || m.ArgsValid != c.wantValid || m.Parallel != c.wantPar || m.NumCalls != c.wantCalls {
			t.Errorf("%s: got %+v", c.name, m)
		}
	}
	// 无工具调用 → 观测错误
	if _, err := ObserveToolCall(adapter.Response{Content: "北京今天 25 度"}, tools); err == nil {
		t.Error("无工具调用应报错")
	}
}

func TestObserveForDispatch(t *testing.T) {
	norm := testNormalizer()
	if _, err := observeFor("needle", adapter.Response{Content: "NEEDLE-ABC23456"}, norm, "none", "",
		[]string{"NEEDLE-ABC23456"}, nil, ""); err != nil {
		t.Errorf("needle 分派失败: %v", err)
	}
	if _, err := observeFor("think-effort", adapter.Response{Usage: adapter.Usage{Reasoning: 10}}, norm, "none", "",
		nil, nil, "tokens"); err != nil {
		t.Errorf("think-effort 分派失败: %v", err)
	}
	if _, err := observeFor("toolcall", adapter.Response{ToolCalls: []adapter.ToolCall{
		{Name: "t", Args: json.RawMessage(`{}`)}}}, norm, "none", "", nil, []string{"t"}, ""); err != nil {
		t.Errorf("toolcall 分派失败: %v", err)
	}
	if _, err := observeFor("future", adapter.Response{}, norm, "none", "", nil, nil, ""); err == nil {
		t.Error("未实现探针应报错")
	}
}
