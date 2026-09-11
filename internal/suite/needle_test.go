package suite

import (
	"strings"
	"testing"
)

// —— needle 标记派生与语料组装 ——

func TestNeedleMarkerDeterministic(t *testing.T) {
	// 同输入恒得同输出（跨运行可复现是观测语义的硬要求）
	m1 := NeedleMarker("nd.v1.001", 0)
	m2 := NeedleMarker("nd.v1.001", 0)
	if m1 != m2 {
		t.Errorf("同输入两次派生不一致: %q vs %q", m1, m2)
	}
	if !strings.HasPrefix(m1, "NEEDLE-") || len(m1) != len("NEEDLE-")+8 {
		t.Errorf("标记形态不对: %q", m1)
	}
	// 字符集：不含易混淆字符
	for _, c := range strings.TrimPrefix(m1, "NEEDLE-") {
		if strings.ContainsRune("IO01", c) {
			t.Errorf("标记含易混淆字符 %q: %q", c, m1)
		}
	}
}

func TestNeedleMarkerDistinct(t *testing.T) {
	// 不同题目、不同位置序号 → 标记必然不同
	seen := map[string]bool{}
	for _, id := range []string{"nd.v1.001", "nd.v1.002", "nd.v1.003"} {
		for i := 0; i < 3; i++ {
			m := NeedleMarker(id, i)
			if seen[m] {
				t.Errorf("标记冲突: %s|%d → %s", id, i, m)
			}
			seen[m] = true
		}
	}
}

func TestNeedleMarkersCount(t *testing.T) {
	ms := NeedleMarkers("q1", []float64{0.25, 0.5, 0.75})
	if len(ms) != 3 {
		t.Fatalf("markers = %v, want 3 个", ms)
	}
	// 下标即位置序号
	for i, m := range ms {
		if m != NeedleMarker("q1", i) {
			t.Errorf("markers[%d] = %q, want %q", i, m, NeedleMarker("q1", i))
		}
	}
}

func TestAssembleNeedleContext(t *testing.T) {
	corpus := strings.Repeat("w", 100) // 100 rune
	out := AssembleNeedleContext(corpus, "q1", []float64{0.25, 0.75})

	m0, m1 := NeedleMarker("q1", 0), NeedleMarker("q1", 1)
	i0 := strings.Index(out, m0)
	i1 := strings.Index(out, m1)
	if i0 < 0 || i1 < 0 {
		t.Fatalf("两个标记都应出现在组装结果里: %q", out)
	}
	if i0 >= i1 {
		t.Errorf("位置 0.25 的标记应在 0.75 之前: i0=%d i1=%d", i0, i1)
	}
	// 埋点在语料中段：标记前有语料、标记后也有语料
	if i0 < 20 || i1 > len(out)-20 {
		t.Errorf("埋点位置不合理: i0=%d i1=%d len=%d", i0, i1, len(out))
	}
	// 包装指令存在
	if !strings.Contains(out, "读到该标记请原样输出："+m0) {
		t.Error("标记应包装成「看到就复述」指令")
	}
	// 确定性：同输入两次组装一致
	if out2 := AssembleNeedleContext(corpus, "q1", []float64{0.25, 0.75}); out2 != out {
		t.Error("组装不确定")
	}
}

func TestAssembleNeedleContextEmptyCorpus(t *testing.T) {
	// bucket=0 退化形态：语料为空，直接拼接指令
	out := AssembleNeedleContext("", "q1", []float64{0.5})
	if !strings.Contains(out, NeedleMarker("q1", 0)) {
		t.Errorf("空语料也应包含标记: %q", out)
	}
	// 无埋点 → 原样返回
	if got := AssembleNeedleContext("abc", "q1", nil); got != "abc" {
		t.Errorf("无埋点 = %q, want abc", got)
	}
}

func TestSortPositions(t *testing.T) {
	got := SortPositions([]float64{0.9, -0.1, 0.5, 1.2})
	want := []float64{0, 0.5, 0.9, 1}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("SortPositions = %v, want %v", got, want)
			break
		}
	}
}

func TestPositionsOfDefault(t *testing.T) {
	ps := ProbeSet{}
	it := Item{ID: "q1"}
	got := ps.PositionsOf(it)
	if len(got) != 1 || got[0] != 0.5 {
		t.Errorf("缺省位置 = %v, want [0.5]", got)
	}
	it2 := Item{ID: "q2", NeedlePositions: []float64{0.75, 0.25}}
	got2 := ps.PositionsOf(it2)
	if len(got2) != 2 || got2[0] != 0.25 || got2[1] != 0.75 {
		t.Errorf("PositionsOf = %v, want 升序 [0.25 0.75]", got2)
	}
}

func TestToolsOfOverride(t *testing.T) {
	ps := ProbeSet{Tools: []Tool{{Name: "probe_level"}}}
	it := Item{ID: "q1"}
	if got := ps.ToolsOf(it); len(got) != 1 || got[0].Name != "probe_level" {
		t.Errorf("题目无 tools 应继承探针级: %v", got)
	}
	it2 := Item{ID: "q2", Tools: []Tool{{Name: "item_level"}}}
	if got := ps.ToolsOf(it2); len(got) != 1 || got[0].Name != "item_level" {
		t.Errorf("题目级 tools 应覆盖: %v", got)
	}
}

// —— 校验 ——

func TestValidateNeedlePositions(t *testing.T) {
	f := &File{SuiteVersion: "v1", Probes: map[string]ProbeSet{
		"needle": {Items: []Item{{ID: "q1", Prompt: "p", NeedlePositions: []float64{1.5}}}},
	}}
	if err := f.Validate(); err == nil || !strings.Contains(err.Error(), "needle_positions") {
		t.Errorf("位置越界应报错: %v", err)
	}
	f.Probes["needle"] = ProbeSet{Items: []Item{{ID: "q1", Prompt: "p", NeedlePositions: []float64{0}}}}
	if err := f.Validate(); err == nil {
		t.Error("位置 0 应报错（开区间）")
	}
}

func TestValidateTools(t *testing.T) {
	cases := []struct {
		name  string
		tools []Tool
		bad   bool
	}{
		{"合法", []Tool{{Name: "t", Parameters: map[string]any{"type": "object"}}}, false},
		{"无 parameters", []Tool{{Name: "t"}}, true},
		{"非 object schema", []Tool{{Name: "t", Parameters: map[string]any{"type": "array"}}}, true},
		{"无 name", []Tool{{Parameters: map[string]any{"type": "object"}}}, true},
		{"重名", []Tool{
			{Name: "t", Parameters: map[string]any{"type": "object"}},
			{Name: "t", Parameters: map[string]any{"type": "object"}},
		}, true},
	}
	for _, c := range cases {
		f := &File{SuiteVersion: "v1", Probes: map[string]ProbeSet{
			"toolcall": {Tools: c.tools, Items: []Item{{ID: "q1", Prompt: "p"}}},
		}}
		err := f.Validate()
		if c.bad && err == nil {
			t.Errorf("%s: 应报错", c.name)
		}
		if !c.bad && err != nil {
			t.Errorf("%s: 不应报错: %v", c.name, err)
		}
	}
}

func TestLoadExampleSuite(t *testing.T) {
	// 仓库随附的示例题库必须能加载并通过校验（五探针题目齐全）
	f, err := Load("../../suites/v1.example.yaml", "")
	if err != nil {
		t.Fatal(err)
	}
	if f.SuiteVersion != "v1" {
		t.Errorf("suite_version = %s, want v1", f.SuiteVersion)
	}
	for _, pid := range []string{"onetoken", "tokenizer", "needle", "think-effort", "toolcall"} {
		ps, ok := f.ProbeSetOf(pid)
		if !ok || len(ps.Items) == 0 {
			t.Errorf("题库缺探针 %s 的题目", pid)
		}
	}
	// needle 题目声明了多埋点
	nd, _ := f.ProbeSetOf("needle")
	if len(nd.Items[0].NeedlePositions) < 2 {
		t.Errorf("nd.example.001 应为多埋点题: %v", nd.Items[0].NeedlePositions)
	}
	// toolcall 探针级工具集非空
	tc, _ := f.ProbeSetOf("toolcall")
	if len(tc.Tools) == 0 {
		t.Error("toolcall 应有探针级工具集")
	}
	// think-effort 题目带强度级别
	te, _ := f.ProbeSetOf("think-effort")
	for _, it := range te.Items {
		if it.ThinkingEffort == nil || *it.ThinkingEffort == "" {
			t.Errorf("题目 %s 应标注 thinking_effort", it.ID)
		}
	}
}
