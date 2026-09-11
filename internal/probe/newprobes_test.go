package probe

import (
	"testing"
)

// —— needle ——

func needleCell(key map[string]string, a, b [][]bool) CellPair {
	mk := func(rows [][]bool) []Observation {
		var out []Observation
		for _, r := range rows {
			out = append(out, obs(map[string]any{"matched": r}))
		}
		return out
	}
	return CellPair{Key: key, A: mk(a), B: mk(b)}
}

func TestNeedleIdenticalRecall(t *testing.T) {
	// 两侧逐埋点命中情况完全一致 → p=1 → pass
	p, _ := Get("needle")
	rows := [][]bool{{true, false, true}, {true, false, true}, {true, true, true}}
	cells := []CellPair{
		needleCell(map[string]string{"question_id": "q1", "context_bucket": "8000"}, rows, rows),
		needleCell(map[string]string{"question_id": "q2", "context_bucket": "8000"}, rows, rows),
		needleCell(map[string]string{"question_id": "q1", "context_bucket": "32000"}, rows, rows),
		needleCell(map[string]string{"question_id": "q2", "context_bucket": "32000"}, rows, rows),
		needleCell(map[string]string{"question_id": "q3", "context_bucket": "8000"}, rows, rows),
	}
	got := p.Compare(cells, 0.01, 0)
	if got.Verdict != "pass" {
		t.Errorf("verdict = %v, want pass (note: %s)", got.Verdict, got.Note)
	}
}

func TestNeedleDivergentRecall(t *testing.T) {
	// A 全召回、B 全漏（如 B 侧上下文被静默截断）→ 显著差异 → fail
	p, _ := Get("needle")
	hits := [][]bool{{true, true}, {true, true}, {true, true}, {true, true}, {true, true}}
	miss := [][]bool{{false, false}, {false, false}, {false, false}, {false, false}, {false, false}}
	cells := []CellPair{
		needleCell(map[string]string{"question_id": "q1", "context_bucket": "128000"}, hits, miss),
		needleCell(map[string]string{"question_id": "q2", "context_bucket": "128000"}, hits, miss),
		needleCell(map[string]string{"question_id": "q3", "context_bucket": "128000"}, hits, miss),
		needleCell(map[string]string{"question_id": "q4", "context_bucket": "128000"}, hits, miss),
		needleCell(map[string]string{"question_id": "q5", "context_bucket": "128000"}, hits, miss),
	}
	got := p.Compare(cells, 0.01, 0)
	if got.Verdict != "fail" {
		t.Errorf("verdict = %v, want fail (note: %s)", got.Verdict, got.Note)
	}
	if got.Statistic == nil || *got.Statistic >= 0.01 {
		t.Errorf("statistic = %v, want < 0.01", got.Statistic)
	}
}

func TestNeedleInsufficientUnits(t *testing.T) {
	// minN=5 但每埋点单元只有 2 个样本 → inconclusive
	p, _ := Get("needle")
	cells := []CellPair{
		needleCell(map[string]string{"question_id": "q1", "context_bucket": "8000"},
			[][]bool{{true}, {false}}, [][]bool{{true}, {false}}),
	}
	got := p.Compare(cells, 0.01, 5)
	if got.Verdict != "inconclusive" {
		t.Errorf("verdict = %v, want inconclusive", got.Verdict)
	}
}

func TestNeedleBothSidesPartial(t *testing.T) {
	// 两侧都不保证全中（参考实现语义）：命中数相同 → 无差异 → pass
	p, _ := Get("needle")
	a := [][]bool{{true, false}, {true, false}, {true, false}}
	b := [][]bool{{true, false}, {true, false}, {true, false}}
	cells := []CellPair{
		needleCell(map[string]string{"question_id": "q1", "context_bucket": "8000"}, a, b),
	}
	got := p.Compare(cells, 0.01, 0)
	if got.Verdict != "pass" {
		t.Errorf("verdict = %v, want pass", got.Verdict)
	}
}

// —— think-effort ——

func teCell(key map[string]string, unit string, a, b []int) CellPair {
	mk := func(xs []int) []Observation {
		var out []Observation
		for _, x := range xs {
			out = append(out, obs(map[string]any{"reasoning_value": x, "unit": unit}))
		}
		return out
	}
	return CellPair{Key: key, A: mk(a), B: mk(b)}
}

func TestThinkEffortSameDistribution(t *testing.T) {
	p, _ := Get("think-effort")
	a := []int{512, 480, 530, 495, 510, 520, 490, 505, 515, 500}
	cells := []CellPair{
		teCell(map[string]string{"question_id": "q1", "context_bucket": "0", "thinking_effort": "high", "protocol": "openai-chat"}, "tokens", a, a),
	}
	got := p.Compare(cells, 0.01, 0)
	if got.Verdict != "pass" {
		t.Errorf("verdict = %v, want pass (note: %s)", got.Verdict, got.Note)
	}
	if got.Cells[0].Extra["unit"] != "tokens" {
		t.Errorf("cell extra 应带单位: %v", got.Cells[0].Extra)
	}
}

func TestThinkEffortBudgetCut(t *testing.T) {
	// B 侧思考预算被削减：high 档 token 数整体低一个量级 → fail
	p, _ := Get("think-effort")
	a := []int{800, 820, 790, 810, 830, 805, 795, 815, 825, 785}
	b := []int{80, 82, 79, 81, 83, 85, 75, 84, 76, 86}
	cells := []CellPair{
		teCell(map[string]string{"question_id": "q1", "context_bucket": "0", "thinking_effort": "high", "protocol": "openai-chat"}, "tokens", a, b),
		teCell(map[string]string{"question_id": "q2", "context_bucket": "0", "thinking_effort": "high", "protocol": "openai-chat"}, "tokens", a, b),
	}
	got := p.Compare(cells, 0.01, 0)
	if got.Verdict != "fail" {
		t.Errorf("verdict = %v, want fail (note: %s)", got.Verdict, got.Note)
	}
	// cell 明细带中位数供报告展示
	if len(got.Cells) != 2 {
		t.Fatalf("cells = %d, want 2", len(got.Cells))
	}
	if got.Cells[0].Extra["a_median"] == nil || got.Cells[0].Extra["b_median"] == nil {
		t.Errorf("cell extra 应含中位数: %v", got.Cells[0].Extra)
	}
}

func TestThinkEffortCharsUnit(t *testing.T) {
	// chars 单位（anthropic 系）：0 是有效观测，B 侧思考文本被截断 → fail
	p, _ := Get("think-effort")
	a := []int{2000, 2100, 1950, 2050, 2020, 1980, 2080, 1990, 2030, 2010}
	b := []int{0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	cells := []CellPair{
		teCell(map[string]string{"question_id": "q1", "context_bucket": "0", "thinking_effort": "high", "protocol": "anthropic-messages"}, "chars", a, b),
	}
	got := p.Compare(cells, 0.01, 0)
	if got.Verdict != "fail" {
		t.Errorf("verdict = %v, want fail（思考文本清零必须能判出）", got.Verdict)
	}
}

func TestThinkEffortUnitMismatch(t *testing.T) {
	// 同 cell 两侧单位不一致（数据异常）→ 该 cell 不参与检验
	p, _ := Get("think-effort")
	mk := func(unit string, xs []int) []Observation {
		var out []Observation
		for _, x := range xs {
			out = append(out, obs(map[string]any{"reasoning_value": x, "unit": unit}))
		}
		return out
	}
	cells := []CellPair{{
		Key: map[string]string{"question_id": "q1", "context_bucket": "0", "thinking_effort": "high", "protocol": "openai-chat"},
		A:   mk("tokens", []int{500, 510, 520, 530, 540}),
		B:   mk("chars", []int{500, 510, 520, 530, 540}),
	}}
	got := p.Compare(cells, 0.01, 0)
	if got.Verdict != "inconclusive" {
		t.Errorf("verdict = %v, want inconclusive", got.Verdict)
	}
	if got.Cells[0].Extra["reason"] != "unit_mismatch" {
		t.Errorf("cell reason = %v, want unit_mismatch", got.Cells[0].Extra)
	}
}

func TestThinkEffortInsufficientSamples(t *testing.T) {
	p, _ := Get("think-effort")
	cells := []CellPair{
		teCell(map[string]string{"question_id": "q1", "context_bucket": "0", "thinking_effort": "low", "protocol": "openai-chat"}, "tokens",
			[]int{10, 20}, []int{10, 20}),
	}
	got := p.Compare(cells, 0.01, 5)
	if got.Verdict != "inconclusive" {
		t.Errorf("verdict = %v, want inconclusive", got.Verdict)
	}
}

// —— toolcall ——

func tcCell(key map[string]string, a, b []toolcallObs) CellPair {
	mk := func(xs []toolcallObs) []Observation {
		var out []Observation
		for _, x := range xs {
			out = append(out, obs(x))
		}
		return out
	}
	return CellPair{Key: key, A: mk(a), B: mk(b)}
}

func tcObs(tool string, valid bool, calls int) toolcallObs {
	return toolcallObs{Tool: tool, ArgsValid: valid, Parallel: calls > 1, NumCalls: calls}
}

func TestToolcallIdentical(t *testing.T) {
	p, _ := Get("toolcall")
	mk := func() []toolcallObs {
		out := []toolcallObs{}
		for i := 0; i < 5; i++ {
			out = append(out, tcObs("get_weather", true, 1))
		}
		for i := 0; i < 5; i++ {
			out = append(out, tcObs("get_forecast", true, 2))
		}
		return out
	}
	cells := []CellPair{
		tcCell(map[string]string{"question_id": "q1", "context_bucket": "0"}, mk(), mk()),
	}
	got := p.Compare(cells, 0.05, 0)
	if got.Verdict != "pass" {
		t.Errorf("verdict = %v, want pass (note: %s)", got.Verdict, got.Note)
	}
}

func TestToolcallDistributionShift(t *testing.T) {
	// 工具选择分布完全不同 → JSD 分量超阈 → fail
	p, _ := Get("toolcall")
	mk := func(tool string) []toolcallObs {
		out := []toolcallObs{}
		for i := 0; i < 10; i++ {
			out = append(out, tcObs(tool, true, 1))
		}
		return out
	}
	cells := []CellPair{
		tcCell(map[string]string{"question_id": "q1", "context_bucket": "0"},
			mk("get_weather"), mk("unit_convert")),
	}
	got := p.Compare(cells, 0.05, 0)
	if got.Verdict != "fail" {
		t.Errorf("verdict = %v, want fail (note: %s)", got.Verdict, got.Note)
	}
}

func TestToolcallValidRateDivergence(t *testing.T) {
	// 工具选择一致但 B 侧参数合法率崩塌 → 合法率卡方分量超阈 → fail
	p, _ := Get("toolcall")
	a := []toolcallObs{}
	b := []toolcallObs{}
	for i := 0; i < 20; i++ {
		a = append(a, tcObs("get_weather", true, 1))
		b = append(b, tcObs("get_weather", i < 5, 1)) // B 侧 75% 参数非法
	}
	cells := []CellPair{
		tcCell(map[string]string{"question_id": "q1", "context_bucket": "0"}, a, b),
	}
	got := p.Compare(cells, 0.05, 0)
	if got.Verdict != "fail" {
		t.Errorf("verdict = %v, want fail (note: %s)", got.Verdict, got.Note)
	}
	// JSD 分量为 0（分布相同），ratio 由合法率分量贡献
	if got.Statistic == nil || *got.Statistic != 0 {
		t.Errorf("statistic = %v, want 0（JSD 均值）", got.Statistic)
	}
	if got.Ratio == nil || *got.Ratio <= 1 {
		t.Errorf("ratio = %v, want > 1", got.Ratio)
	}
}

func TestToolcallInsufficient(t *testing.T) {
	p, _ := Get("toolcall")
	cells := []CellPair{
		tcCell(map[string]string{"question_id": "q1", "context_bucket": "0"},
			[]toolcallObs{tcObs("a", true, 1)}, []toolcallObs{tcObs("a", true, 1)}),
	}
	got := p.Compare(cells, 0.05, 5)
	if got.Verdict != "inconclusive" {
		t.Errorf("verdict = %v, want inconclusive", got.Verdict)
	}
}

func TestNewProbesMeta(t *testing.T) {
	// 注册表齐五探针；元信息与规格一致
	for id, wantSchema := range map[string]string{
		"needle":       "needle/v1",
		"think-effort": "think-effort/v2",
		"toolcall":     "toolcall/v1",
	} {
		p, ok := Get(id)
		if !ok {
			t.Fatalf("探针 %s 未注册", id)
		}
		m := p.Meta()
		if m.ObservationSchema != wantSchema {
			t.Errorf("%s schema = %s, want %s", id, m.ObservationSchema, wantSchema)
		}
	}
	te, _ := Get("think-effort")
	teMeta := te.Meta()
	if teMeta.StatKind != StatPValue {
		t.Errorf("think-effort StatKind = %v, want pvalue", teMeta.StatKind)
	}
	if teMeta.Version != "2" {
		t.Errorf("think-effort Version = %s, want 2", teMeta.Version)
	}
	wantKey := []string{"question_id", "context_bucket", "thinking_effort", "protocol"}
	if len(teMeta.CellKey) != len(wantKey) {
		t.Fatalf("think-effort CellKey = %v, want %v", teMeta.CellKey, wantKey)
	}
	for i := range wantKey {
		if teMeta.CellKey[i] != wantKey[i] {
			t.Errorf("think-effort CellKey = %v, want %v（按强度与协议分层）", teMeta.CellKey, wantKey)
			break
		}
	}
}
