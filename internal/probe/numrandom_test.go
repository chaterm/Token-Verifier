package probe

import (
	"math"
	"testing"
)

func makeNumRandomObs(vals []int, minVal, maxVal, target int) Observation {
	return obs(map[string]any{
		"values":       vals,
		"min_value":    minVal,
		"max_value":    maxVal,
		"target_count": target,
	})
}

func synthSeq(start, step, count, minVal, maxVal int) []int {
	width := maxVal - minVal + 1
	out := make([]int, count)
	for i := 0; i < count; i++ {
		out[i] = minVal + ((start + i*step) % width)
	}
	return out
}

func TestNumRandomIdenticalAndShifted(t *testing.T) {
	p, ok := Get("numrandom")
	if !ok {
		t.Fatal("numrandom 未注册")
	}
	if p.Meta().MinN != 1 || p.Meta().StatKind != StatDistance {
		t.Fatalf("Meta = %+v", p.Meta())
	}

	seq1 := synthSeq(7, 37, 300, 1, 355)
	seq2 := synthSeq(19, 53, 300, 1, 355)
	cellsSame := []CellPair{
		{
			Key: map[string]string{"question_id": "nr.1", "context_bucket": "0"},
			A:   []Observation{makeNumRandomObs(seq1, 1, 355, 300)},
			B:   []Observation{makeNumRandomObs(seq1, 1, 355, 300)},
		},
		{
			Key: map[string]string{"question_id": "nr.2", "context_bucket": "0"},
			A:   []Observation{makeNumRandomObs(seq2, 1, 355, 300)},
			B:   []Observation{makeNumRandomObs(seq2, 1, 355, 300)},
		},
	}
	vSame := p.Compare(cellsSame, 0.15, 1)
	if vSame.Verdict != "pass" || vSame.Statistic == nil || *vSame.Statistic != 0 || vSame.Ratio == nil || *vSame.Ratio != 0 {
		t.Fatalf("完全相同序列应返回 pass 且统计量为 0: %+v", vSame)
	}

	// 显著偏移：A 集中在 [1..30]，B 集中在 [320..355]
	lowSeq := make([]int, 300)
	highSeq := make([]int, 300)
	for i := 0; i < 300; i++ {
		lowSeq[i] = 1 + (i % 30)
		highSeq[i] = 320 + (i % 36)
	}
	cellsDiff := []CellPair{
		{
			Key: map[string]string{"question_id": "nr.1", "context_bucket": "0"},
			A:   []Observation{makeNumRandomObs(lowSeq, 1, 355, 300)},
			B:   []Observation{makeNumRandomObs(highSeq, 1, 355, 300)},
		},
	}
	vDiff := p.Compare(cellsDiff, 0.15, 1)
	if vDiff.Verdict != "fail" || vDiff.Statistic == nil || *vDiff.Statistic <= 0.5 {
		t.Fatalf("不相交值域应显著 fail: %+v", vDiff)
	}
	wantRatio := *vDiff.Statistic / 0.15
	if math.Abs(*vDiff.Ratio-wantRatio) > 1e-12 {
		t.Errorf("Ratio = %v, want Statistic/Threshold = %v", *vDiff.Ratio, wantRatio)
	}
	if len(vDiff.Cells) != 1 || vDiff.Cells[0].Extra["a_total_ints"] != 300 {
		t.Errorf("CellDetail.Extra 缺少证据字段: %+v", vDiff.Cells)
	}
}

func TestNumRandomCoverageGateAndOrderInvariance(t *testing.T) {
	p, ok := Get("numrandom")
	if !ok {
		t.Fatal("numrandom 未注册")
	}
	sA1 := synthSeq(3, 17, 300, 1, 355)
	sA2 := synthSeq(11, 29, 300, 1, 355)
	sB1 := synthSeq(5, 19, 300, 1, 355)
	sB2 := synthSeq(13, 31, 300, 1, 355)

	// 同一 cell 内记录顺序对调 + cell 顺序对调，统计量严格不变
	c1 := []CellPair{
		{
			Key: map[string]string{"question_id": "nr.1", "context_bucket": "0"},
			A:   []Observation{makeNumRandomObs(sA1, 1, 355, 300), makeNumRandomObs(sA2, 1, 355, 300)},
			B:   []Observation{makeNumRandomObs(sB1, 1, 355, 300), makeNumRandomObs(sB2, 1, 355, 300)},
		},
		{
			Key: map[string]string{"question_id": "nr.2", "context_bucket": "0"},
			A:   []Observation{makeNumRandomObs(sA2, 1, 355, 300)},
			B:   []Observation{makeNumRandomObs(sB2, 1, 355, 300)},
		},
	}
	c2 := []CellPair{
		c1[1],
		{
			Key: map[string]string{"question_id": "nr.1", "context_bucket": "0"},
			A:   []Observation{makeNumRandomObs(sA2, 1, 355, 300), makeNumRandomObs(sA1, 1, 355, 300)},
			B:   []Observation{makeNumRandomObs(sB2, 1, 355, 300), makeNumRandomObs(sB1, 1, 355, 300)},
		},
	}
	v1 := p.Compare(c1, 0.15, 1)
	v2 := p.Compare(c2, 0.15, 1)
	if v1.Statistic == nil || v2.Statistic == nil || math.Abs(*v1.Statistic-*v2.Statistic) > 1e-15 {
		t.Fatalf("池化结果受记录/cell 顺序影响: %v vs %v", v1.Statistic, v2.Statistic)
	}

	// 覆盖率门控：5 个配对 cell 中仅 3 个达标（60% < 80%），即使达标 cell 完全相同也必须判 inconclusive（防假 pass）
	fiveCells := make([]CellPair, 5)
	for i := 0; i < 5; i++ {
		pair := CellPair{Key: map[string]string{"question_id": string(rune('a' + i)), "context_bucket": "0"}}
		if i < 3 {
			pair.A = []Observation{makeNumRandomObs(sA1, 1, 355, 300)}
			pair.B = []Observation{makeNumRandomObs(sA1, 1, 355, 300)}
		} else {
			pair.A = []Observation{makeNumRandomObs(sA1, 1, 355, 300)}
			pair.B = nil // B 侧缺失
		}
		fiveCells[i] = pair
	}
	vLowCov := p.Compare(fiveCells, 0.15, 1)
	if vLowCov.Verdict != "inconclusive" || vLowCov.Statistic != nil || vLowCov.Ratio != nil {
		t.Fatalf("达标 cell < 80%% 应判 inconclusive 且清空 Statistic/Ratio: %+v", vLowCov)
	}

	// 值域不匹配 → inconclusive
	vMismatch := p.Compare([]CellPair{{
		Key: map[string]string{"question_id": "nr.1", "context_bucket": "0"},
		A:   []Observation{makeNumRandomObs(sA1, 1, 355, 300)},
		B:   []Observation{makeNumRandomObs(sA1, 1, 200, 300)},
	}}, 0.15, 1)
	if vMismatch.Verdict != "inconclusive" {
		t.Fatalf("值域不一致应判 inconclusive: %+v", vMismatch)
	}
}

func TestNumRandomRunawayLegacySequenceCapped(t *testing.T) {
	p, ok := Get("numrandom")
	if !ok {
		t.Fatal("numrandom 未注册")
	}
	// 模拟线上真实事故：B 侧前 300 个整数与 A 完全一致，但随后失控多写出 2 万个整数。
	// 比较端必须按 target_count=300 截断，否则失控尾部会把分布距离放大成假阳性。
	normal := synthSeq(7, 37, 300, 1, 355)
	runaway := append([]int{}, normal...)
	for i := 0; i < 20000; i++ {
		runaway = append(runaway, 1+(i%355))
	}
	cells := []CellPair{{
		Key: map[string]string{"question_id": "nr.1", "context_bucket": "0"},
		A:   []Observation{makeNumRandomObs(normal, 1, 355, 300)},
		B:   []Observation{makeNumRandomObs(runaway, 1, 355, 300)},
	}}
	v := p.Compare(cells, 0.15, 1)
	if v.Verdict != "pass" || v.Statistic == nil || *v.Statistic != 0 {
		t.Fatalf("失控生成应按 target_count 截断并判 pass(0): %+v", v)
	}
	if got := v.Cells[0].Extra["a_total_ints"]; got != 300 {
		t.Errorf("a_total_ints = %v, want 300", got)
	}
	if got := v.Cells[0].Extra["b_total_ints"]; got != 300 {
		t.Errorf("b_total_ints = %v, want 300（失控尾部应被截断）", got)
	}
}
