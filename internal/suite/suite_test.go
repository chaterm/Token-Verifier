package suite

import (
	"os"
	"strings"
	"testing"
)

// vocabFile 带词表与扩展表的题库：归一化空间由题库提供。
func vocabFile() *File {
	return &File{
		SuiteVersion: "v1",
		Normalize: NormalizeSpace{
			Maps: map[string]map[string][]string{
				"color": {
					"red":    {"red", "红", "rouge"},
					"blue":   {"blue", "蓝", "bleu"},
					"orange": {"orange", "橙"},
					"maroon": {"maroon", "栗色"},
				},
			},
			Digits:  map[string]int{"⑦": 7},
			Letters: map[string]string{"é": "e"},
		},
	}
}

func TestNormalizeNumber(t *testing.T) {
	n := vocabFile().Normalizer()
	cases := map[string]string{
		"42":       "42",
		"  答案是 7。": "7",
		"大约３点":     "3",  // 全角数字
		"٧":        "7",  // 阿拉伯-印度数字
		"七":        "7",  // 中文数字
		"四十二":      "42", // 中文复合数词
		"一百二十三":    "123",
		"十五":       "15", // 省略一
		"一万两千":     "12000",
		"⑦号":       "7", // 题库 digits 扩展
		"3.5 左右":   "3.5",
		"第3.14章":   "3.14",
		"没有任何数字":   "",
	}
	for in, want := range cases {
		if got := n.Apply("number", in); got != want {
			t.Errorf("number(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeLetter(t *testing.T) {
	n := vocabFile().Normalizer()
	if got := n.Apply("letter", "答案是 B"); got != "b" {
		t.Errorf("letter = %q, want b", got)
	}
	if got := n.Apply("letter", "xyz"); got != "x" {
		t.Errorf("letter = %q, want x", got)
	}
	if got := n.Apply("letter", "选 É"); got != "e" {
		t.Errorf("题库 letters 扩展 = %q, want e", got)
	}
	if got := n.Apply("letter", "答案是Ｂ"); got != "b" {
		t.Errorf("全角字母 = %q, want b", got)
	}
}

func TestNormalizeVocabDeterministic(t *testing.T) {
	n := vocabFile().Normalizer()
	cases := map[string]string{
		"红色":     "red",
		"Blue":   "blue",
		"我说蓝色":   "blue",
		"bleu":   "blue",
		"orange": "orange",
		// 最长匹配胜出：栗色（maroon）
		"栗色":       "maroon",
		"一种不存在的颜色": "一种不存在的颜色",
	}
	for in, want := range cases {
		if got := n.Apply("color", in); got != want {
			t.Errorf("color(%q) = %q, want %q", in, got, want)
		}
	}
	// 确定性：同一输入多次运行结果一致（与 map 遍历序无关）。
	// red 与 orange 同长（3/6），"red-orange" 两个词都命中，
	// 最长者 orange 恒胜出。
	for i := 0; i < 50; i++ {
		if got := n.Apply("color", "red-orange"); got != "orange" {
			t.Fatalf("复合色 red-orange = %q, want orange（最长匹配、确定）", got)
		}
	}
}

func TestNormalizeWordAndNone(t *testing.T) {
	n := vocabFile().Normalizer()
	if got := n.Apply("word", "Hello World"); got != "hello" {
		t.Errorf("word = %q, want hello", got)
	}
	if got := n.Apply("none", "  原样  "); got != "原样" {
		t.Errorf("none = %q, want 原样（仅去空白）", got)
	}
	// 未知规则兜底：原样返回
	if got := n.Apply("bogus", " x "); got != "x" {
		t.Errorf("unknown = %q", got)
	}
}

func TestLoadAndValidate(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/s.yaml"
	content := `suite_version: v1
output_contract:
  field: answer
  system_prompt: '只回答 JSON {"answer": "..."}'
normalize:
  maps:
    color:
      red: [red, 红]
probes:
  onetoken:
    normalize: number
    items:
      - id: ot.v1.001
        prompt: "说一个数"
      - id: ot.v1.002
        prompt: "说一种颜色"
        normalize: color
`
	writeFile(t, p, content)
	f, err := Load(p, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if f.SuiteVersion != "v1" {
		t.Errorf("SuiteVersion = %q", f.SuiteVersion)
	}
	items := f.ItemsOf("onetoken")
	if len(items) != 2 || items[0].ID != "ot.v1.001" {
		t.Errorf("items = %+v", items)
	}
	// color 是题库定义的词表规则 → 可用
	if !f.KnownRule("color") {
		t.Error("color 应为已知规则（题库已定义词表）")
	}
}

func TestValidateUnknownNormalize(t *testing.T) {
	f := &File{
		SuiteVersion: "v1",
		Probes: map[string]ProbeSet{
			// color 未在 normalize.maps 定义 → 应报错
			"onetoken": {Normalize: "color", Items: []Item{{ID: "a", Prompt: "x"}}},
		},
	}
	if err := f.Validate(); err == nil {
		t.Fatal("未定义词表的规则应报错")
	}
	f2 := &File{
		SuiteVersion: "v1",
		Probes: map[string]ProbeSet{
			"onetoken": {Normalize: "bogus", Items: []Item{{ID: "a", Prompt: "x"}}},
		},
	}
	if err := f2.Validate(); err == nil {
		t.Fatal("未知 normalize 应报错")
	}
}

func TestValidateNormalizeSpace(t *testing.T) {
	// digits 键非单字符 → 报错
	f := &File{SuiteVersion: "v1", Normalize: NormalizeSpace{Digits: map[string]int{"①②": 1}}}
	if err := f.Validate(); err == nil {
		t.Fatal("digits 多字符键应报错")
	}
	// digits 值越界 → 报错
	f2 := &File{SuiteVersion: "v1", Normalize: NormalizeSpace{Digits: map[string]int{"①": 12}}}
	if err := f2.Validate(); err == nil {
		t.Fatal("digits 值越界应报错")
	}
	// 空词表 → 报错
	f3 := &File{SuiteVersion: "v1", Normalize: NormalizeSpace{Maps: map[string]map[string][]string{"color": {}}}}
	if err := f3.Validate(); err == nil {
		t.Fatal("空词表应报错")
	}
}

func TestLoadBadSHA256(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/s.yaml"
	writeFile(t, p, "suite_version: v1\nprobes: {}\n")
	_, err := Load(p, "deadbeef")
	if err == nil {
		t.Fatal("sha256 不匹配应报错")
	}
}

func TestValidateDuplicateID(t *testing.T) {
	f := &File{
		SuiteVersion: "v1",
		Probes: map[string]ProbeSet{
			"onetoken": {Normalize: "number", Items: []Item{
				{ID: "a", Prompt: "x"}, {ID: "a", Prompt: "y"},
			}},
		},
	}
	if err := f.Validate(); err == nil {
		t.Fatal("重复 id 应报错")
	}
}

func TestGeneratePaddingDeterministic(t *testing.T) {
	a := GeneratePadding(8000, DefaultPaddingSeed)
	b := GeneratePadding(8000, DefaultPaddingSeed)
	if a != b {
		t.Fatal("同一 bucket 同一 seed 应生成同一文本")
	}
	if GeneratePadding(0, DefaultPaddingSeed) != "" {
		t.Fatal("bucket 0 应为空")
	}
	if GeneratePadding(8000, DefaultPaddingSeed) == GeneratePadding(16000, DefaultPaddingSeed) {
		t.Fatal("不同 bucket 应生成不同文本")
	}
}

func TestGeneratePaddingSeed(t *testing.T) {
	// 默认 seed 与 padding/v1 时代的算法逐字节一致（黄金值取自旧实现）
	if got := GeneratePadding(1, DefaultPaddingSeed); got != "how" {
		t.Errorf("bucket=1: %q, want \"how\"", got)
	}
	if got := GeneratePadding(2, DefaultPaddingSeed); got != "of" {
		t.Errorf("bucket=2: %q, want \"of\"", got)
	}
	want := "was all the of she with do how which up of how about what with can we but each but they by at an is other can as all whe"
	if got := GeneratePadding(8000, DefaultPaddingSeed); !strings.HasPrefix(got, want) {
		t.Errorf("bucket=8000 前缀不匹配: got %q, want %q...", got[:min(len(got), 120)], want)
	}
	// 不同 seed → 不同序列（同 bucket）
	if GeneratePadding(8000, DefaultPaddingSeed) == GeneratePadding(8000, 999) {
		t.Error("不同 seed 应生成不同文本")
	}
	// 同 seed 跨调用确定
	a, b := GeneratePadding(8000, 999), GeneratePadding(8000, 999)
	if a != b {
		t.Error("同 seed 应确定")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// —— 输出契约（system prompt 强约束）——

func contractFile() *File {
	return &File{
		SuiteVersion: "v2",
		OutputContract: &OutputContract{
			Field:        "answer",
			SystemPrompt: `请只回答一个合法的 JSON 对象，格式为 {"answer": "<your answer>"}。`,
		},
		Probes: map[string]ProbeSet{
			"onetoken":  {Normalize: "number", Items: []Item{{ID: "a", Prompt: "x"}}},
			"tokenizer": {Normalize: "none", Items: []Item{{ID: "b", Prompt: "y"}}},
		},
	}
}

func TestContractForMechanicalRule(t *testing.T) {
	f := contractFile()
	// normalize ≠ none/空 → 应用契约
	for _, rule := range []string{"number", "letter", "word", "color"} {
		if got := f.ContractFor(rule); got == nil {
			t.Errorf("ContractFor(%q) = nil, want 契约", rule)
		}
	}
	// none/空 → raw，不发 system prompt
	if got := f.ContractFor("none"); got != nil {
		t.Errorf("ContractFor(none) = %+v, want nil", got)
	}
	if got := f.ContractFor(""); got != nil {
		t.Errorf("ContractFor(空) = %+v, want nil", got)
	}
	// 题库没定义契约段时，任何规则都返回 nil（Validate 会拦这种缺失）
	bare := &File{SuiteVersion: "v2"}
	if got := bare.ContractFor("number"); got != nil {
		t.Errorf("无契约段 ContractFor = %+v, want nil", got)
	}
}

func TestContractFieldDefault(t *testing.T) {
	f := &File{SuiteVersion: "v2", OutputContract: &OutputContract{SystemPrompt: "sp"}}
	if got := f.ContractFor("number").FieldOf(); got != "answer" {
		t.Errorf("Field 空默认 = %q, want answer", got)
	}
	f.OutputContract.Field = "ans"
	if got := f.ContractFor("number").FieldOf(); got != "ans" {
		t.Errorf("FieldOf = %q, want ans", got)
	}
}

func TestValidateRequiresContractWhenNormalized(t *testing.T) {
	// 有 normalize 题目但缺 output_contract → 报错
	f := &File{
		SuiteVersion: "v2",
		Probes: map[string]ProbeSet{
			"onetoken": {Normalize: "number", Items: []Item{{ID: "a", Prompt: "x"}}},
		},
	}
	if err := f.Validate(); err == nil {
		t.Fatal("缺 output_contract 应报错")
	}
	// system_prompt 为空 → 报错
	f2 := contractFile()
	f2.OutputContract.SystemPrompt = "  "
	if err := f2.Validate(); err == nil {
		t.Fatal("system_prompt 为空应报错")
	}
	// 全部 normalize=none → 不需要契约，通过
	f3 := &File{
		SuiteVersion: "v2",
		Probes: map[string]ProbeSet{
			"tokenizer": {Normalize: "none", Items: []Item{{ID: "b", Prompt: "y"}}},
		},
	}
	if err := f3.Validate(); err != nil {
		t.Fatalf("纯 raw 题库不应要求契约: %v", err)
	}
	// 题目级 normalize 触发（探针级 none 但题目 color）→ 也需要契约
	f4 := &File{
		SuiteVersion: "v2",
		Probes: map[string]ProbeSet{
			"p": {Normalize: "none", Items: []Item{{ID: "c", Prompt: "z", Normalize: "word"}}},
		},
	}
	if err := f4.Validate(); err == nil {
		t.Fatal("题目级 normalize 也应触发契约要求")
	}
}

func TestLoadContractFromYAML(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/s.yaml"
	content := `suite_version: v2
output_contract:
  field: answer
  system_prompt: '请只回答 JSON {"answer": "..."}'
probes:
  onetoken:
    normalize: number
    items:
      - id: ot.v2.001
        prompt: "说一个数"
`
	writeFile(t, p, content)
	f, err := Load(p, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if f.OutputContract == nil || f.OutputContract.SystemPrompt == "" {
		t.Fatalf("output_contract 未加载: %+v", f.OutputContract)
	}
	if got := f.ContractFor("number"); got == nil {
		t.Error("ContractFor(number) = nil")
	}
}
