package plan

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/chaterm/token-verifier/internal/config"
	"github.com/chaterm/token-verifier/internal/suite"
)

// minimalCfg 返回一份指向 minSuite 的最小配置（已 Load、已补默认）。
func minimalCfg(t *testing.T, st *suite.File) *config.File {
	return &config.File{
		Version: 1,
		Target: config.Target{
			BaseURL:   "https://api.example.com",
			APIKeyEnv: "X",
			Model:     "m",
			Protocols: []config.ProtocolConfig{
				{ID: "openai-chat", Weight: 1.0,
					Transport:    config.TransportMix{Stream: 1.0},
					Capabilities: config.Capabilities{Stream: true, Tools: true, Thinking: true},
					Thinking: config.ThinkingBlock{
						Enabled: config.ThinkingMode{EffortMap: map[string]any{"low": "low"}},
					},
				},
			},
			Request: config.RequestConfig{TemperatureField: "temperature"},
		},
		Probes: map[string]config.ProbeConfig{
			"onetoken": {
				Enabled: true, Repeats: 20,
				Temperature:    f64ptr(1.0),
				TopP:           nil,
				MaxTokens:      intptr(16),
				ContextBuckets: []int{0, 32000},
			},
			"tokenizer": {
				Enabled: true, Repeats: 1,
				Temperature:    f64ptr(0.0),
				MaxTokens:      intptr(16),
				ContextBuckets: []int{0},
			},
			"needle": {Enabled: false, Repeats: 5, ContextBuckets: []int{32000}},
		},
	}
}

// minSuite 一份题库。
func minSuite() *suite.File {
	return &suite.File{
		SuiteVersion: "v1",
		Probes: map[string]suite.ProbeSet{
			"onetoken": {Normalize: "number", Items: []suite.Item{
				{ID: "ot.v1.002", Prompt: "p2"},
				{ID: "ot.v1.001", Prompt: "p1"},
			}},
			"tokenizer": {Normalize: "none", Items: []suite.Item{
				{ID: "tk.v1.001", Prompt: "t1"},
			}},
		},
	}
}

func f64ptr(f float64) *float64 { return &f }
func intptr(i int) *int         { return &i }

func TestBuildPlanShape(t *testing.T) {
	st := minSuite()
	cfg := minimalCfg(t, st)
	cp, err := Build(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	// 未实现/未启用的探针不进计划
	if _, ok := cp.Probes["needle"]; ok {
		t.Fatal("needle 未启用却进了计划")
	}
	ot := cp.Probes["onetoken"]
	if ot.Repeats != 20 || ot.ObservationSchema != "onetoken/v2" {
		t.Errorf("onetoken plan = %+v", ot)
	}
	if len(ot.QuestionIDs) != 2 || ot.QuestionIDs[0] != "ot.v1.001" {
		t.Errorf("question_ids 应按字典序排序: %v", ot.QuestionIDs)
	}
	tk := cp.Probes["tokenizer"]
	if len(tk.CellKey) != 3 || tk.CellKey[2] != "protocol" {
		t.Errorf("tokenizer cell_key 必须含 protocol: %v", tk.CellKey)
	}
	if cp.QuestionSetDigest == "" || !strings.HasPrefix(cp.QuestionSetDigest, "sha256:") {
		t.Errorf("question_set_digest = %q", cp.QuestionSetDigest)
	}
	if cp.PaddingAlgoVersion != suite.PaddingAlgoVersion {
		t.Errorf("padding_algo_version = %q", cp.PaddingAlgoVersion)
	}
}

func TestBuildQuestionSetDigest(t *testing.T) {
	st := minSuite()
	cfg := minimalCfg(t, st)
	cp1, _ := Build(cfg, st)
	st2 := minSuite()
	ps := st2.Probes["onetoken"]
	ps.Items = append(ps.Items, suite.Item{ID: "ot.v1.099", Prompt: "x"})
	st2.Probes["onetoken"] = ps
	cp2, _ := Build(cfg, st2)
	if cp1.QuestionSetDigest == cp2.QuestionSetDigest {
		t.Fatal("增删题目应改变 question_set_digest")
	}
	// 同 id 换文本、同版本：digest 不变（静默错配由使用者负责递增版本）
	st3 := minSuite()
	st3.Probes["onetoken"].Items[0].Prompt = "改过的文本"
	cp3, _ := Build(cfg, st3)
	if cp1.QuestionSetDigest != cp3.QuestionSetDigest {
		t.Fatal("同 id 换文本在 suite_version 不变时不应改变 question_set_digest")
	}
}

func TestDigestStabilityAndSensitivity(t *testing.T) {
	st := minSuite()
	cfg := minimalCfg(t, st)
	cp1, _ := Build(cfg, st)
	d1, _, err := cp1.Digest()
	if err != nil {
		t.Fatal(err)
	}
	cp2, _ := Build(cfg, st)
	d2, _, _ := cp2.Digest()
	if d1 != d2 {
		t.Fatal("同一配置两次构建 digest 应相同")
	}

	// 改探针参数 → digest 变
	cfg2 := minimalCfg(t, st)
	p := cfg2.Probes["onetoken"]
	p.Repeats = 40
	cfg2.Probes["onetoken"] = p
	cp3, _ := Build(cfg2, st)
	d3, _, _ := cp3.Digest()
	if d1 == d3 {
		t.Fatal("repeats 变化应改变 plan_digest")
	}

	// target 段是网关接线细节，整体不进 digest：
	// capabilities / thinking / weight / base_url / temperature_field / body_overrides
	// 怎么变，digest 都不应改变（档位与流式的裁剪效果体现在 sampling/skipped，
	// 每题的思考档位由题目级 thinking_effort 承载、已随题库进 digest）
	cfg4 := minimalCfg(t, st)
	cfg4.Target.BaseURL = "https://other.example.com"
	cfg4.Target.Protocols[0].Weight = 0.2
	cfg4.Target.Protocols[0].Capabilities.ContextWindow = 999
	cfg4.Target.Protocols[0].Thinking.Enabled.EffortMap["low"] = "medium"
	cfg4.Target.Request.TemperatureField = "generation_config.temperature"
	cfg4.Target.Request.BodyOverrides = map[string]any{"top_k": 40}
	cp5, _ := Build(cfg4, st)
	d5, _, _ := cp5.Digest()
	if d1 != d5 {
		t.Fatal("target 段（capabilities/thinking/weight/base_url/request）不应改变 plan_digest")
	}
}

func TestProbeDigest(t *testing.T) {
	st := minSuite()
	cfg := minimalCfg(t, st)
	cp, _ := Build(cfg, st)
	otDigest, err := cp.ProbeDigest("onetoken")
	if err != nil {
		t.Fatal(err)
	}
	tkDigest, _ := cp.ProbeDigest("tokenizer")
	if otDigest == tkDigest {
		t.Fatal("不同探针的 probe_digest 应不同")
	}

	// probe_digest 只覆盖探针计划块，不折协议的 thinking 块（网关接线细节）
	cfg2 := minimalCfg(t, st)
	cfg2.Target.Protocols[0].Thinking.Enabled.EffortMap["low"] = "medium"
	cp2, _ := Build(cfg2, st)
	ot2, _ := cp2.ProbeDigest("onetoken")
	if ot2 != otDigest {
		t.Fatal("改 effort_map 不应改变 probe_digest")
	}

	// 改探针自身的参数 → probe_digest 变
	cfg3 := minimalCfg(t, st)
	cfg3.Probes["onetoken"] = config.ProbeConfig{
		Enabled: true, Repeats: 40, Temperature: f64ptr(1.0), MaxTokens: intptr(16),
		ContextBuckets: []int{0, 32000},
	}
	cp3, _ := Build(cfg3, st)
	ot3, _ := cp3.ProbeDigest("onetoken")
	if ot3 == otDigest {
		t.Fatal("改 repeats 应改变 onetoken 的 probe_digest")
	}
	tk3, _ := cp3.ProbeDigest("tokenizer")
	if tk3 != tkDigest {
		t.Fatal("onetoken 参数变化不应影响 tokenizer 的 probe_digest")
	}
}

func TestCanonicalJSON(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"键序", map[string]any{"b": 1, "a": 2}, `{"a":2,"b":1}`},
		{"嵌套键序", map[string]any{"z": map[string]any{"y": 1, "x": 2}}, `{"z":{"x":2,"y":1}}`},
		{"无空白", map[string]any{"a": []any{1, 2, 3}}, `{"a":[1,2,3]}`},
		{"整数无小数点", json.Number("1.0"), "1"},
		{"浮点最短", 0.15, "0.15"},
		{"显式null保留", map[string]any{"a": nil}, `{"a":null}`},
		{"数组顺序有意义", []any{3, 1, 2}, `[3,1,2]`},
		{"字符串转义", map[string]any{"s": "a\nb"}, `{"s":"a\nb"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CanonicalJSON(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("CanonicalJSON(%v) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

// 顶层指针 null 与缺失键必须产生不同结果的前提是：构建侧总是保留字段为 null
// （如 top_p: null）。这里验证 CanonicalJSON 对指针 nil 与缺键的区别。
type ptrProbe struct {
	TopP *float64 `json:"top_p"`
}

func TestCanonicalJSONPtrNullKept(t *testing.T) {
	b1, _ := CanonicalJSON(ptrProbe{nil})
	if string(b1) != `{"top_p":null}` {
		t.Errorf("指针 nil 应产出显式 null，得到 %s", b1)
	}
}

func TestNormalizeSpaceInDigest(t *testing.T) {
	// 归一化词表进 plan_digest：改词表 = 改观测语义
	st := minSuite()
	cfg := minimalCfg(t, st)
	cp1, _ := Build(cfg, st)
	d1, _, _ := cp1.Digest()

	st2 := minSuite()
	st2.Normalize.Maps = map[string]map[string][]string{"color": {"red": {"红"}}}
	cp2, _ := Build(cfg, st2)
	d2, _, _ := cp2.Digest()
	if d1 == d2 {
		t.Fatal("改归一化词表应改变 plan_digest")
	}

	// canonical JSON 中 normalize 段存在且空表为 {}
	canon, _ := CanonicalJSON(cp1)
	if !strings.Contains(string(canon), `"normalize":{"digits":{},"letters":{},"maps":{}}`) {
		t.Errorf("canonical JSON 应含空 normalize 段: %s", canon)
	}
}

func TestProtocolWeightZeroExcludedFromPlan(t *testing.T) {
	// weight=0 的协议不进 plan（不影响采集内容，不应影响 digest）
	st := minSuite()
	cfg := minimalCfg(t, st)
	cfg.Target.Protocols = append(cfg.Target.Protocols, config.ProtocolConfig{
		ID: "anthropic-messages", Weight: 0,
		Transport: config.TransportMix{Stream: 1.0},
	})
	cp1, _ := Build(cfg, st)
	d1, _, _ := cp1.Digest()

	cfg2 := minimalCfg(t, st)
	cp2, _ := Build(cfg2, st)
	d2, _, _ := cp2.Digest()
	if d1 != d2 {
		t.Fatal("weight=0 的协议不应改变 plan_digest")
	}
}

// —— 输出契约进 digest ——

func TestBuildOutputContractInPlan(t *testing.T) {
	st := minSuite()
	st.OutputContract = &suite.OutputContract{Field: "answer", SystemPrompt: "只回答 JSON"}
	cfg := minimalCfg(t, st)
	cp, err := Build(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	if cp.OutputContract == nil {
		t.Fatal("output_contract 应进计划")
	}
	if cp.OutputContract.Field != "answer" || cp.OutputContract.SystemPrompt != "只回答 JSON" {
		t.Errorf("output_contract = %+v", cp.OutputContract)
	}
	// canonical JSON 里应有 output_contract 键
	_, canonical, err := cp.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(canonical), `"output_contract"`) {
		t.Errorf("canonical 缺 output_contract: %s", canonical)
	}
}

func TestBuildOutputContractDigestSensitivity(t *testing.T) {
	st := minSuite()
	st.OutputContract = &suite.OutputContract{Field: "answer", SystemPrompt: "文本 A"}
	cfg := minimalCfg(t, st)
	cpA, _ := Build(cfg, st)
	dA, _, err := cpA.Digest()
	if err != nil {
		t.Fatal(err)
	}
	// 契约文本变了 → digest 必须变（改契约 = 改观测语义）
	st.OutputContract.SystemPrompt = "文本 B"
	cpB, _ := Build(cfg, st)
	dB, _, err := cpB.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if dA == dB {
		t.Error("契约文本不同，plan_digest 应不同")
	}
	// 题库没有契约（纯 raw）→ output_contract 为 null，Build 不报错
	st.OutputContract = nil
	cpC, err := Build(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	if cpC.OutputContract != nil {
		t.Errorf("无契约题库 output_contract = %+v, want nil", cpC.OutputContract)
	}
}

func TestOnetokenProbeVersionBumpForContract(t *testing.T) {
	// 观测提取语义变更（JSON 解包）→ onetoken probe_version/observation_schema 递增
	st := minSuite()
	st.OutputContract = &suite.OutputContract{SystemPrompt: "sp"}
	cfg := minimalCfg(t, st)
	cp, err := Build(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	ot := cp.Probes["onetoken"]
	if ot.ProbeVersion != "2" || ot.ObservationSchema != "onetoken/v2" {
		t.Errorf("onetoken = %s / %s, want 2 / onetoken/v2", ot.ProbeVersion, ot.ObservationSchema)
	}
	tk := cp.Probes["tokenizer"]
	if tk.ProbeVersion != "1" || tk.ObservationSchema != "tokenizer/v1" {
		t.Errorf("tokenizer 应保持 1 / tokenizer/v1: %s / %s", tk.ProbeVersion, tk.ObservationSchema)
	}
}

func TestProbePlanMinN(t *testing.T) {
	st := minSuite()
	cfg := minimalCfg(t, st)
	cp, err := Build(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	if cp.Probes["onetoken"].MinN != config.DefaultMinN["onetoken"] {
		t.Errorf("缺省 min_n = %d, want %d", cp.Probes["onetoken"].MinN, config.DefaultMinN["onetoken"])
	}
	if cp.Probes["tokenizer"].MinN != config.DefaultMinN["tokenizer"] {
		t.Errorf("tokenizer 缺省 min_n = %d, want %d", cp.Probes["tokenizer"].MinN, config.DefaultMinN["tokenizer"])
	}

	// 自定义值进 plan，且 digest 敏感
	cfg2 := minimalCfg(t, st)
	n := 5
	cfg2.Probes["onetoken"] = config.ProbeConfig{
		Enabled: true, Repeats: 20, Temperature: f64ptr(1.0), MaxTokens: intptr(16),
		ContextBuckets: []int{0, 32000}, MinN: &n,
	}
	cp2, err := Build(cfg2, st)
	if err != nil {
		t.Fatal(err)
	}
	if cp2.Probes["onetoken"].MinN != 5 {
		t.Errorf("min_n = %d, want 5", cp2.Probes["onetoken"].MinN)
	}
	d1, _, _ := cp.Digest()
	d2, _, _ := cp2.Digest()
	if d1 == d2 {
		t.Error("min_n 不同 → digest 必须不同")
	}
}

func TestPaddingSeedInPlan(t *testing.T) {
	st := minSuite()
	cfg := minimalCfg(t, st)
	cp, err := Build(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	// 缺省：生效值为 suite.DefaultPaddingSeed，进 plan（digest 的一部分）
	if cp.PaddingSeed != suite.DefaultPaddingSeed {
		t.Errorf("padding_seed = %d, want %d", cp.PaddingSeed, suite.DefaultPaddingSeed)
	}
	if cp.PaddingAlgoVersion != "padding/v2" {
		t.Errorf("padding_algo_version = %q, want padding/v2", cp.PaddingAlgoVersion)
	}

	// 自定义 seed → plan 变化且 digest 敏感
	cfg2 := minimalCfg(t, st)
	s := int64(999)
	cfg2.Padding.Seed = &s
	cp2, _ := Build(cfg2, st)
	if cp2.PaddingSeed != 999 {
		t.Errorf("padding_seed = %d, want 999", cp2.PaddingSeed)
	}
	d1, _, _ := cp.Digest()
	d2, _, _ := cp2.Digest()
	if d1 == d2 {
		t.Error("padding seed 不同 → digest 必须不同")
	}
}
