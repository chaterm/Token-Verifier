package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTemp 落一个临时 YAML 并 Load。
func writeTemp(t *testing.T, content string) *File {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tv.yaml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return f
}

// baseYAML 一份最小合法配置；各测试在此基础上改。
const baseYAML = `version: 1
target:
  base_url: https://api.example.com
  api_key_env: TV_TEST_KEY
  model: example-model
  protocols:
    - id: openai-chat
      weight: 1.0
      transport: { stream: 1.0, non_stream: 0.0 }
suite:
  path: `

// makeSuite 配一份带 onetoken 题的 suite，返回其路径。
func makeSuite(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "suite.yaml")
	content := `suite_version: v1
probes:
  onetoken:
    normalize: number
    items:
      - id: ot.v1.001
        prompt: "说一个数"
`
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// suiteItemsOf 从 makeSuite 的内容构造校验用的题目信息。
func onetokenItems() map[string][]SuiteItemInfo {
	return map[string][]SuiteItemInfo{
		"onetoken": {{ID: "ot.v1.001"}},
	}
}

func TestMain(m *testing.M) {
	os.Setenv("TV_TEST_KEY", "sk-test-dummy")
	os.Exit(m.Run())
}

func TestValidateOK(t *testing.T) {
	cfg := writeTemp(t, baseYAML+makeSuite(t)+`
probes:
  onetoken: { enabled: true, repeats: 20, temperature: 1.0, max_tokens: 16, context_buckets: [0] }
`)
	warns := []string{}
	if err := cfg.Validate(onetokenItems(), func(s string) { warns = append(warns, s) }); err != nil {
		t.Fatalf("合法配置不应报错: %v", err)
	}
	if len(warns) != 0 {
		t.Errorf("不应有警告: %v", warns)
	}
}

func TestValidateStructuralErrors(t *testing.T) {
	suitePath := makeSuite(t)
	cases := []struct {
		name    string
		mutate  func(y string) string
		wantSub string
	}{
		{"版本错", func(y string) string { return strings.Replace(y, "version: 1", "version: 2", 1) }, "version"},
		{"base_url空", func(y string) string { return strings.Replace(y, "https://api.example.com", "", 1) }, "base_url"},
		{"base_url非法", func(y string) string { return strings.Replace(y, "https://api.example.com", "ftp://x", 1) }, "base_url"},
		{"api_key_env空", func(y string) string { return strings.Replace(y, "TV_TEST_KEY", "", 1) }, "api_key_env"},
		{"model空", func(y string) string { return strings.Replace(y, "example-model", "", 1) }, "model"},
		{"协议未知", func(y string) string { return strings.Replace(y, "openai-chat", "bogus-proto", 1) }, "未知协议"},
		{"transport全零", func(y string) string {
			return strings.Replace(y, "stream: 1.0, non_stream: 0.0", "stream: 0.0, non_stream: 0.0", 1)
		}, "transport"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			y := tc.mutate(baseYAML + suitePath + `
probes:
  onetoken: { enabled: true, repeats: 20, temperature: 1.0, max_tokens: 16, context_buckets: [0] }
`)
			cfg := writeTemp(t, y)
			err := cfg.Validate(onetokenItems(), func(string) {})
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("err = %v, want 含 %q", err, tc.wantSub)
			}
		})
	}
}

func TestValidateAPIKeyEnvMissing(t *testing.T) {
	os.Unsetenv("TV_TEST_KEY")
	defer os.Setenv("TV_TEST_KEY", "sk-test-dummy")
	cfg := writeTemp(t, baseYAML+makeSuite(t)+`
probes:
  onetoken: { enabled: true, repeats: 20, temperature: 1.0, max_tokens: 16, context_buckets: [0] }
`)
	if err := cfg.Validate(onetokenItems(), func(string) {}); err == nil {
		t.Fatal("环境变量未设置应报错")
	}
}

func TestValidateProbeErrors(t *testing.T) {
	suitePath := makeSuite(t)
	cases := []struct {
		name    string
		probes  string
		wantSub string
	}{
		{"无启用探针", "onetoken: { enabled: false }", "至少一个探针"},
		{"repeats零", "onetoken: { enabled: true, repeats: 0, context_buckets: [0] }", "repeats"},
		{"buckets空", "onetoken: { enabled: true, repeats: 1 }", "context_buckets"},
		{"buckets非递增", "onetoken: { enabled: true, repeats: 1, context_buckets: [100, 50] }", "严格递增"},
		{"题库无题", "tokenizer: { enabled: true, repeats: 1, context_buckets: [0] }", "不存在该探针"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := writeTemp(t, baseYAML+suitePath+"\nprobes:\n  "+tc.probes+"\n")
			err := cfg.Validate(onetokenItems(), func(string) {})
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("err = %v, want 含 %q", err, tc.wantSub)
			}
		})
	}
}

func TestValidateEffortMapMissingLevel(t *testing.T) {
	suitePath := makeSuite(t)
	cfg := writeTemp(t, baseYAML+suitePath+`
probes:
  think-effort: { enabled: true, repeats: 10, context_buckets: [0] }
`)
	items := map[string][]SuiteItemInfo{
		"think-effort": {{ID: "te.1", ThinkingEffort: "high"}},
	}
	err := cfg.Validate(items, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "effort_map") {
		t.Errorf("缺 effort_map 级别应报错: %v", err)
	}
}

func TestValidateReservedFields(t *testing.T) {
	suitePath := makeSuite(t)
	y := `version: 1
target:
  base_url: https://api.example.com
  api_key_env: TV_TEST_KEY
  model: m
  protocols:
    - id: openai-chat
      weight: 1.0
      transport: { stream: 1.0, non_stream: 0.0 }
  request:
    body_overrides: { model: hacked }
suite:
  path: ` + suitePath + `
probes:
  onetoken: { enabled: true, repeats: 20, context_buckets: [0] }
`
	cfg := writeTemp(t, y)
	err := cfg.Validate(onetokenItems(), func(string) {})
	if err == nil || !strings.Contains(err.Error(), "保留字段") {
		t.Errorf("body_overrides 含 model 应报错: %v", err)
	}
}

func TestValidateTemperatureFieldConflict(t *testing.T) {
	suitePath := makeSuite(t)
	y := `version: 1
target:
  base_url: https://api.example.com
  api_key_env: TV_TEST_KEY
  model: m
  protocols:
    - id: openai-chat
      weight: 1.0
      transport: { stream: 1.0, non_stream: 0.0 }
      thinking:
        enabled:
          effort_map: { low: low }
          body_overrides: { temperature: "${thinking_effort}" }
  request:
    temperature_field: temperature
suite:
  path: ` + suitePath + `
probes:
  onetoken: { enabled: true, repeats: 20, context_buckets: [0] }
`
	cfg := writeTemp(t, y)
	err := cfg.Validate(onetokenItems(), func(string) {})
	if err == nil || !strings.Contains(err.Error(), "同一路径") {
		t.Errorf("temperature_field 冲突应报错: %v", err)
	}
}

func TestValidatePlaceholderInDisabled(t *testing.T) {
	suitePath := makeSuite(t)
	y := `version: 1
target:
  base_url: https://api.example.com
  api_key_env: TV_TEST_KEY
  model: m
  protocols:
    - id: openai-chat
      weight: 1.0
      transport: { stream: 1.0, non_stream: 0.0 }
      thinking:
        disabled:
          body_overrides: { foo: "${thinking_effort}" }
suite:
  path: ` + suitePath + `
probes:
  onetoken: { enabled: true, repeats: 20, context_buckets: [0] }
`
	cfg := writeTemp(t, y)
	err := cfg.Validate(onetokenItems(), func(string) {})
	if err == nil || !strings.Contains(err.Error(), "${thinking_effort}") {
		t.Errorf("disabled 块含占位符应报错: %v", err)
	}
}

func TestValidateWarnings(t *testing.T) {
	suitePath := makeSuite(t)
	cfg := writeTemp(t, baseYAML+suitePath+`
probes:
  onetoken: { enabled: true, repeats: 3, temperature: 0.2, context_buckets: [0] }
`)
	warns := []string{}
	if err := cfg.Validate(onetokenItems(), func(s string) { warns = append(warns, s) }); err != nil {
		t.Fatalf("警告不应阻断: %v", err)
	}
	if len(warns) != 2 {
		t.Errorf("应有两条警告（温度过低 + repeats 不足），得到 %v", warns)
	}
}

func TestCapabilitiesExplicitFalseNotFlipped(t *testing.T) {
	// 显式写全 false（纯文本端点）是合法配置，不能被缺省值翻转成全 true
	y := `version: 1
target:
  base_url: https://api.example.com
  api_key_env: TV_TEST_KEY
  model: m
  protocols:
    - id: openai-chat
      weight: 1.0
      capabilities: { stream: false, tools: false, thinking: false, context_window: 0 }
      transport: { stream: 0.0, non_stream: 1.0 }
suite:
  path: ` + makeSuite(t) + `
probes:
  onetoken: { enabled: true, repeats: 20, temperature: 1.0, max_tokens: 16, context_buckets: [0] }
`
	cfg := writeTemp(t, y)
	c := cfg.Target.Protocols[0].Capabilities
	if c.Stream || c.Tools || c.Thinking {
		t.Errorf("显式全 false 被翻转: %+v", c)
	}
	if err := cfg.Validate(onetokenItems(), func(string) {}); err != nil {
		t.Errorf("显式全 false 应通过校验: %v", err)
	}
}

func TestCapabilitiesDefaultsAndPartial(t *testing.T) {
	// 整段缺省 → 全 true
	y1 := `version: 1
target:
  base_url: https://x.example.com
  api_key_env: TV_TEST_KEY
  model: m
  protocols:
    - id: openai-chat
      weight: 1.0
      transport: { non_stream: 1.0 }
suite:
  path: ` + makeSuite(t) + `
probes:
  onetoken: { enabled: true, repeats: 20, temperature: 1.0, max_tokens: 16, context_buckets: [0] }
`
	cfg1 := writeTemp(t, y1)
	c1 := cfg1.Target.Protocols[0].Capabilities
	if !c1.Stream || !c1.Tools || !c1.Thinking || c1.ContextWindow != 0 {
		t.Errorf("缺省应为全 true + 窗口 0: %+v", c1)
	}

	// 部分给出：只写 stream: false，其余取缺省 true
	y2 := `version: 1
target:
  base_url: https://x.example.com
  api_key_env: TV_TEST_KEY
  model: m
  protocols:
    - id: openai-chat
      weight: 1.0
      capabilities: { stream: false }
      transport: { non_stream: 1.0 }
suite:
  path: ` + makeSuite(t) + `
probes:
  onetoken: { enabled: true, repeats: 20, temperature: 1.0, max_tokens: 16, context_buckets: [0] }
`
	cfg2 := writeTemp(t, y2)
	c2 := cfg2.Target.Protocols[0].Capabilities
	if c2.Stream || !c2.Tools || !c2.Thinking {
		t.Errorf("部分给出: stream=false 其余 true，得到 %+v", c2)
	}
}

func TestValidateRawLevelFullRejected(t *testing.T) {
	y := `version: 1
target:
  base_url: https://api.example.com
  api_key_env: TV_TEST_KEY
  model: m
  protocols:
    - id: openai-chat
      weight: 1.0
      transport: { stream: 1.0 }
suite:
  path: ` + makeSuite(t) + `
probes:
  onetoken: { enabled: true, repeats: 20, temperature: 1.0, max_tokens: 16, context_buckets: [0] }
runtime:
  raw_level: full
`
	cfg := writeTemp(t, y)
	err := cfg.Validate(onetokenItems(), func(string) {})
	if err == nil || !strings.Contains(err.Error(), "raw_level") {
		t.Errorf("full 档未实现应明确拒绝: %v", err)
	}
}

func TestValidateRawLevelBogus(t *testing.T) {
	y := `version: 1
target:
  base_url: https://api.example.com
  api_key_env: TV_TEST_KEY
  model: m
  protocols:
    - id: openai-chat
      weight: 1.0
      transport: { stream: 1.0 }
suite:
  path: ` + makeSuite(t) + `
probes:
  onetoken: { enabled: true, repeats: 20, temperature: 1.0, max_tokens: 16, context_buckets: [0] }
runtime:
  raw_level: bogus
`
	cfg := writeTemp(t, y)
	if err := cfg.Validate(onetokenItems(), func(string) {}); err == nil {
		t.Fatal("未知 raw_level 应报错")
	}
}

func TestValidateReservedFieldSystem(t *testing.T) {
	// system 是输出契约的注入点（anthropic 形态为顶层字段），不许被 overrides 覆盖
	suitePath := makeSuite(t)
	y := `version: 1
target:
  base_url: https://api.example.com
  api_key_env: TV_TEST_KEY
  model: m
  protocols:
    - id: openai-chat
      weight: 1.0
      transport: { stream: 1.0, non_stream: 0.0 }
  request:
    body_overrides: { system: hacked }
suite:
  path: ` + suitePath + `
probes:
  onetoken: { enabled: true, repeats: 20, context_buckets: [0] }
`
	cfg := writeTemp(t, y)
	err := cfg.Validate(onetokenItems(), func(string) {})
	if err == nil || !strings.Contains(err.Error(), "保留字段") {
		t.Errorf("body_overrides 含 system 应报错: %v", err)
	}
}

func TestValidateMinN(t *testing.T) {
	suitePath := makeSuite(t)
	// 显式 min_n < 1 → 报错
	cfg := writeTemp(t, baseYAML+suitePath+`
probes:
  onetoken: { enabled: true, repeats: 20, temperature: 1.0, context_buckets: [0], min_n: 0 }
`)
	if err := cfg.Validate(onetokenItems(), func(string) {}); err == nil || !strings.Contains(err.Error(), "min_n") {
		t.Errorf("min_n=0 应报错: %v", err)
	}

	// 自定义 min_n 生效：repeats=5 ≥ min_n=5 → 无警告（默认 min_n=10 时会警告）
	cfg = writeTemp(t, baseYAML+suitePath+`
probes:
  onetoken: { enabled: true, repeats: 5, temperature: 1.0, context_buckets: [0], min_n: 5 }
`)
	warns := []string{}
	if err := cfg.Validate(onetokenItems(), func(s string) { warns = append(warns, s) }); err != nil {
		t.Fatal(err)
	}
	if len(warns) != 0 {
		t.Errorf("repeats 已达自定义 min_n，不应警告: %v", warns)
	}

	// 未写 min_n → 解析为 nil（由调用方取 DefaultMinN）
	cfg = writeTemp(t, baseYAML+suitePath+`
probes:
  onetoken: { enabled: true, repeats: 20, temperature: 1.0, context_buckets: [0] }
`)
	if cfg.Probes["onetoken"].MinN != nil {
		t.Errorf("缺省 min_n 应为 nil: %v", *cfg.Probes["onetoken"].MinN)
	}
}

func TestDefaultMinN(t *testing.T) {
	if DefaultMinN["onetoken"] != 10 || DefaultMinN["tokenizer"] != 1 {
		t.Errorf("DefaultMinN = %v, want onetoken:10 tokenizer:1", DefaultMinN)
	}
}

func TestPaddingSeed(t *testing.T) {
	// 未写 padding 段 → nil（调用方取 suite.DefaultPaddingSeed）
	cfg := writeTemp(t, "version: 1\n")
	if cfg.Padding.Seed != nil {
		t.Errorf("缺省 padding.seed 应为 nil: %v", *cfg.Padding.Seed)
	}
	// 写了 padding.seed → 解析生效
	cfg = writeTemp(t, "version: 1\npadding:\n  seed: 42\n")
	if cfg.Padding.Seed == nil || *cfg.Padding.Seed != 42 {
		t.Errorf("padding.seed = %v, want 42", cfg.Padding.Seed)
	}
	// seed: 0 是显式合法值（与未写区分）
	cfg = writeTemp(t, "version: 1\npadding:\n  seed: 0\n")
	if cfg.Padding.Seed == nil || *cfg.Padding.Seed != 0 {
		t.Errorf("padding.seed=0 应解析为显式 0: %v", cfg.Padding.Seed)
	}
}

func TestValidateThinkEffortPerProtocolWarning(t *testing.T) {
	// think-effort 的 cell 按协议分层：weight 0.2 × repeats 10 = 期望 2 < min_n 5
	// → 该协议应有警告；weight 0.8 × 10 = 8 ≥ 5 → 不警告
	suitePath := filepath.Join(t.TempDir(), "te-suite.yaml")
	if err := os.WriteFile(suitePath, []byte(`suite_version: v1
probes:
  think-effort:
    normalize: none
    items:
      - id: te.v1.001
        prompt: "想清楚再答"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := writeTemp(t, `version: 1
target:
  base_url: https://api.example.com
  api_key_env: TV_TEST_KEY
  model: m
  protocols:
    - id: openai-chat
      weight: 0.8
      transport: { stream: 0.0, non_stream: 1.0 }
    - id: anthropic-messages
      weight: 0.2
      transport: { stream: 1.0, non_stream: 0.0 }
suite:
  path: `+suitePath+`
probes:
  think-effort: { enabled: true, repeats: 10, temperature: 1.0, context_buckets: [0] }
`)
	items := map[string][]SuiteItemInfo{"think-effort": {{ID: "te.v1.001"}}}
	var warns []string
	if err := cfg.Validate(items, func(s string) { warns = append(warns, s) }); err != nil {
		t.Fatalf("警告不应阻断: %v", err)
	}
	found := false
	for _, w := range warns {
		if strings.Contains(w, "anthropic-messages") && strings.Contains(w, "think-effort") {
			found = true
		}
		if strings.Contains(w, "openai-chat") && strings.Contains(w, "期望样本") {
			t.Errorf("openai-chat 份额 8 ≥ 5，不应有分层警告: %s", w)
		}
	}
	if !found {
		t.Errorf("anthropic-messages 期望样本 2 < min_n 5 应有警告，得到 %v", warns)
	}
}
