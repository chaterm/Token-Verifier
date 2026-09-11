package probe

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func obs(v any) Observation {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func TestOnetokenIdenticalDistributions(t *testing.T) {
	// 两侧分布完全一致 → JSD=0 → pass
	p, _ := Get("onetoken")
	var a, b []Observation
	for _, v := range []string{"7", "7", "7", "3", "3", "5", "1", "9", "2", "8"} {
		a = append(a, obs(map[string]string{"value": v}))
		b = append(b, obs(map[string]string{"value": v}))
	}
	got := p.Compare([]CellPair{{Key: map[string]string{"question_id": "q1", "context_bucket": "0"}, A: a, B: b}}, 0.15, 0)
	if got.Verdict != "pass" {
		t.Errorf("verdict = %v, want pass", got.Verdict)
	}
	if got.Statistic == nil || *got.Statistic != 0 {
		t.Errorf("statistic = %v, want 0", got.Statistic)
	}
	if got.Ratio == nil || *got.Ratio != 0 {
		t.Errorf("ratio = %v, want 0", got.Ratio)
	}
}

func TestOnetokenDisjointDistributions(t *testing.T) {
	// 两侧取值完全不相交 → 每 cell JSD=1 → 均值 1 > 0.15 → fail
	p, _ := Get("onetoken")
	mk := func(vals ...string) []Observation {
		var out []Observation
		for _, v := range vals {
			out = append(out, obs(map[string]string{"value": v}))
		}
		return out
	}
	cells := []CellPair{
		{Key: map[string]string{"question_id": "q1", "context_bucket": "0"},
			A: mk("1", "1", "1", "1", "1", "1", "1", "1", "1", "1"),
			B: mk("2", "2", "2", "2", "2", "2", "2", "2", "2", "2")},
		{Key: map[string]string{"question_id": "q2", "context_bucket": "0"},
			A: mk("3", "3", "3", "3", "3", "4", "4", "4", "4", "4"),
			B: mk("5", "5", "5", "5", "5", "6", "6", "6", "6", "6")},
	}
	got := p.Compare(cells, 0.15, 0)
	if got.Verdict != "fail" {
		t.Errorf("verdict = %v, want fail", got.Verdict)
	}
	if !almostEq(*got.Statistic, 1.0) {
		t.Errorf("statistic = %v, want 1.0", *got.Statistic)
	}
	// ratio = statistic/threshold > 1 ⟺ fail
	if *got.Ratio <= 1 {
		t.Errorf("ratio = %v, want >1", *got.Ratio)
	}
	if len(got.Cells) != 2 {
		t.Errorf("cells = %d, want 2", len(got.Cells))
	}
}

func TestOnetokenInsufficientSamples(t *testing.T) {
	// 单侧样本 < 10 → cell 标 insufficient；全部不足 → inconclusive
	p, _ := Get("onetoken")
	var a, b []Observation
	for i := 0; i < 5; i++ {
		a = append(a, obs(map[string]string{"value": "7"}))
		b = append(b, obs(map[string]string{"value": "7"}))
	}
	got := p.Compare([]CellPair{{Key: map[string]string{"question_id": "q1", "context_bucket": "0"}, A: a, B: b}}, 0.15, 0)
	if got.Verdict != "inconclusive" {
		t.Errorf("verdict = %v, want inconclusive", got.Verdict)
	}
	if len(got.Cells) != 1 || !got.Cells[0].Insufficient {
		t.Errorf("cell 应标 insufficient: %+v", got.Cells)
	}
	if got.Statistic != nil {
		t.Errorf("inconclusive 时 statistic 应为 nil")
	}

	// 混合：一个达标 cell 正常参与统计，不足的被剔除
	var big []Observation
	for i := 0; i < 12; i++ {
		big = append(big, obs(map[string]string{"value": "7"}))
	}
	got = p.Compare([]CellPair{
		{Key: map[string]string{"question_id": "q1", "context_bucket": "0"}, A: a, B: b},
		{Key: map[string]string{"question_id": "q2", "context_bucket": "0"}, A: big, B: big},
	}, 0.15, 0)
	if got.Verdict != "pass" {
		t.Errorf("verdict = %v, want pass", got.Verdict)
	}
	if got.Cells[0].Insufficient != true || got.Cells[1].Insufficient != false {
		t.Errorf("insufficient 标记错误: %+v", got.Cells)
	}
}

func TestOnetokenNoPairs(t *testing.T) {
	p, _ := Get("onetoken")
	got := p.Compare(nil, 0.15, 0)
	if got.Verdict != "inconclusive" || got.Note == "" {
		t.Errorf("got %+v, want inconclusive with note", got)
	}
}

func TestOnetokenParseFailure(t *testing.T) {
	// 观测 value 类型错误（数字而非字符串）→ json 解析失败 → 计入 parse_failed；
	// 失败过多致样本不足 → insufficient
	p, _ := Get("onetoken")
	var a, b []Observation
	for i := 0; i < 10; i++ {
		a = append(a, obs(map[string]string{"value": "7"}))
		b = append(b, obs(map[string]int{"value": 123})) // 类型不符，解析报错
	}
	got := p.Compare([]CellPair{{Key: map[string]string{"question_id": "q1", "context_bucket": "0"}, A: a, B: b}}, 0.15, 0)
	if len(got.Cells) != 1 || !got.Cells[0].Insufficient {
		t.Fatalf("cells = %+v, want 1 insufficient cell", got.Cells)
	}
	if got.Cells[0].Extra["parse_failed"] != 10 {
		t.Errorf("parse_failed = %v, want 10", got.Cells[0].Extra["parse_failed"])
	}
}

func TestTokenizerAllMatch(t *testing.T) {
	// 全部相等 → p=1 → pass
	p, _ := Get("tokenizer")
	pairs := []CellPair{
		{Key: map[string]string{"question_id": "t1", "context_bucket": "0", "protocol": "openai-chat"},
			A: []Observation{obs(map[string]int{"prompt_tokens": 128})},
			B: []Observation{obs(map[string]int{"prompt_tokens": 128})}},
		{Key: map[string]string{"question_id": "t2", "context_bucket": "0", "protocol": "openai-chat"},
			A: []Observation{obs(map[string]int{"prompt_tokens": 256})},
			B: []Observation{obs(map[string]int{"prompt_tokens": 256})}},
	}
	got := p.Compare(pairs, 0.01, 0)
	if got.Verdict != "pass" {
		t.Errorf("verdict = %v, want pass", got.Verdict)
	}
	if got.Statistic == nil || !almostEq(*got.Statistic, 1) {
		t.Errorf("statistic = %v, want p=1", got.Statistic)
	}
	// p 值类 ratio = alpha/p = 0.01 < 1 ⟺ pass
	if *got.Ratio > 1 {
		t.Errorf("ratio = %v, want <=1", *got.Ratio)
	}
}

func TestTokenizerAllMismatch(t *testing.T) {
	// 全不等（换分词器场景）→ p 很小 → α=0.01 下 fail（n≥5 的功效区间）
	p, _ := Get("tokenizer")
	for _, n := range []int{5, 10, 20} {
		var pairs []CellPair
		for i := 0; i < n; i++ {
			pairs = append(pairs, CellPair{
				Key: map[string]string{"question_id": "t" + itoaTest(i), "context_bucket": "0", "protocol": "openai-chat"},
				A:   []Observation{obs(map[string]int{"prompt_tokens": 100 + i})},
				B:   []Observation{obs(map[string]int{"prompt_tokens": 200 + i})},
			})
		}
		got := p.Compare(pairs, 0.01, 0)
		if got.Verdict != "fail" {
			t.Errorf("n=%d: verdict = %v (p=%v), want fail", n, got.Verdict, *got.Statistic)
		}
		if *got.Ratio <= 1 {
			t.Errorf("n=%d: ratio = %v, want >1", n, *got.Ratio)
		}
	}
}

func TestTokenizerLowPowerRegime(t *testing.T) {
	// 2×2 表的固有功效边界：全不等时 n=1 → p=1，n=4 → p≈0.029，
	// α=0.01 下都判不出 fail；n<5 必须带功效不足警告。
	p, _ := Get("tokenizer")
	build := func(n int) []CellPair {
		var pairs []CellPair
		for i := 0; i < n; i++ {
			pairs = append(pairs, CellPair{
				Key: map[string]string{"question_id": "t" + itoaTest(i), "context_bucket": "0", "protocol": "openai-chat"},
				A:   []Observation{obs(map[string]int{"prompt_tokens": 100 + i})},
				B:   []Observation{obs(map[string]int{"prompt_tokens": 200 + i})},
			})
		}
		return pairs
	}
	for _, n := range []int{1, 2, 3, 4} {
		got := p.Compare(build(n), 0.01, 0)
		if got.Verdict != "pass" {
			t.Errorf("n=%d: verdict = %v (p=%v)，低功效区间应 pass", n, got.Verdict, *got.Statistic)
		}
		if !strings.Contains(got.Warning, "功效不足") {
			t.Errorf("n=%d: warning 应含功效不足提示: %q", n, got.Warning)
		}
	}
	// n=1 时 p 恰为 1
	one := p.Compare(build(1), 0.01, 0)
	if !almostEq(*one.Statistic, 1) {
		t.Errorf("n=1: p = %v, want 1", *one.Statistic)
	}
	// n=5 起进入功效区间，且不再带警告
	five := p.Compare(build(5), 0.01, 0)
	if five.Verdict != "fail" {
		t.Errorf("n=5: verdict = %v (p=%v), want fail", five.Verdict, *five.Statistic)
	}
	if five.Warning != "" {
		t.Errorf("n=5: 不应带功效警告: %q", five.Warning)
	}
}

func TestTokenizerFisherConservativeBoundary(t *testing.T) {
	// Fisher 回退的保守性边界（设计取舍，测试固化行为）：
	// 10 cell 中 5 个不等 → 期望频数不足走 Fisher → p≈0.033 → α=0.01 下 pass。
	p, _ := Get("tokenizer")
	var pairs []CellPair
	for i := 0; i < 10; i++ {
		bv := 100 + i
		if i < 5 {
			bv = 900 + i
		}
		pairs = append(pairs, CellPair{
			Key: map[string]string{"question_id": "t" + itoaTest(i), "context_bucket": "0", "protocol": "openai-chat"},
			A:   []Observation{obs(map[string]int{"prompt_tokens": 100 + i})},
			B:   []Observation{obs(map[string]int{"prompt_tokens": bv})},
		})
	}
	got := p.Compare(pairs, 0.01, 0)
	if !strings.Contains(got.Note, "method=fisher") {
		t.Errorf("失配数 5/10 应走 Fisher: %q", got.Note)
	}
	if got.Verdict != "pass" {
		t.Errorf("verdict = %v (p=%v)，Fisher 在此边界偏保守应为 pass", got.Verdict, *got.Statistic)
	}
}

func TestTokenizerSparseMismatchRobust(t *testing.T) {
	// 20 题里零星 1~2 题不等 → 不误报
	p, _ := Get("tokenizer")
	build := func(mismatches int) []CellPair {
		var pairs []CellPair
		for i := 0; i < 20; i++ {
			bv := 100 + i
			if i < mismatches {
				bv = 999 + i
			}
			pairs = append(pairs, CellPair{
				Key: map[string]string{"question_id": "t" + itoaTest(i), "context_bucket": "0", "protocol": "openai-chat"},
				A:   []Observation{obs(map[string]int{"prompt_tokens": 100 + i})},
				B:   []Observation{obs(map[string]int{"prompt_tokens": bv})},
			})
		}
		return pairs
	}
	for _, m := range []int{1, 2} {
		got := p.Compare(build(m), 0.01, 0)
		if got.Verdict != "pass" {
			t.Errorf("mismatches=%d: verdict = %v (p=%v), want pass", m, got.Verdict, *got.Statistic)
		}
	}
}

func TestTokenizerRepeatsMultiset(t *testing.T) {
	// repeats>1：同 cell 多个观测按多重集比较
	p, _ := Get("tokenizer")
	pairs := []CellPair{{
		Key: map[string]string{"question_id": "t1", "context_bucket": "0", "protocol": "openai-chat"},
		// 顺序无关的多重集相等
		A: []Observation{obs(map[string]int{"prompt_tokens": 10}), obs(map[string]int{"prompt_tokens": 20})},
		B: []Observation{obs(map[string]int{"prompt_tokens": 20}), obs(map[string]int{"prompt_tokens": 10})},
	}}
	got := p.Compare(pairs, 0.01, 0)
	if got.Cells[0].Extra["match"] != true {
		t.Errorf("多重集相等应判 match: %+v", got.Cells[0])
	}
}

func TestTokenizerMissingObservation(t *testing.T) {
	// 观测缺失/解析失败 → cell 不计入；全部不可用 → inconclusive
	p, _ := Get("tokenizer")
	got := p.Compare([]CellPair{{
		Key: map[string]string{"question_id": "t1", "context_bucket": "0", "protocol": "openai-chat"},
		A:   []Observation{obs(map[string]int{"prompt_tokens": 10})},
		B:   nil,
	}}, 0.01, 0)
	if got.Verdict != "inconclusive" {
		t.Errorf("verdict = %v, want inconclusive", got.Verdict)
	}
	if !got.Cells[0].Insufficient {
		t.Error("观测不可用的 cell 应标 insufficient")
	}
}

func TestRatioPValueZero(t *testing.T) {
	// p 恰为 0 时 ratio 不能是 +Inf（JSON 无法表示），用 MaxFloat64 顶替
	r := ratio(StatPValue, 0, 0.01)
	if math.IsInf(r, 1) || r != math.MaxFloat64 {
		t.Errorf("ratio(0 p) = %v, want MaxFloat64", r)
	}
}

func almostEq(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func TestOnetokenCustomMinN(t *testing.T) {
	// min_n 由调用方传入（来自采集计划）；<1 时回退探针默认
	p, _ := Get("onetoken")
	var a, b []Observation
	for i := 0; i < 3; i++ {
		a = append(a, obs(map[string]string{"value": "7"}))
		b = append(b, obs(map[string]string{"value": "7"}))
	}
	pair := []CellPair{{Key: map[string]string{"question_id": "q1", "context_bucket": "0"}, A: a, B: b}}

	if got := p.Compare(pair, 0.15, 0); got.Verdict != "inconclusive" {
		t.Errorf("minN=0 应回退默认 10 → inconclusive, got %v", got.Verdict)
	}
	if got := p.Compare(pair, 0.15, -1); got.Verdict != "inconclusive" {
		t.Errorf("minN<1 应回退默认 → inconclusive, got %v", got.Verdict)
	}
	got := p.Compare(pair, 0.15, 3)
	if got.Verdict != "pass" {
		t.Errorf("minN=3 时 3 个样本应达标: verdict = %v, want pass", got.Verdict)
	}
	if len(got.Cells) != 1 || got.Cells[0].Insufficient {
		t.Errorf("cell 不应标 insufficient: %+v", got.Cells)
	}
	// inconclusive 提示语应反映生效的 minN
	got = p.Compare(pair, 0.15, 5)
	if got.Verdict != "inconclusive" || !strings.Contains(got.Note, "n < 5") {
		t.Errorf("note 应含生效 minN: %q", got.Note)
	}
}
