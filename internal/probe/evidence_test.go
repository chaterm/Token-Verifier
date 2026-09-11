package probe

import (
	"testing"

	"github.com/chaterm/token-verifier/internal/stats"
)

// evidence_test.go：CellDetail.Extra 里的证据数据（分布 / 直方图），
// 供 verbose 报告渲染 —— 只增数据，不改判定语义。

func TestOnetokenCellCarriesDistributions(t *testing.T) {
	p, _ := Get("onetoken")
	var a, b []Observation
	for _, v := range []string{"7", "7", "7", "3", "3", "5", "1", "9", "2", "8"} {
		a = append(a, obs(map[string]string{"value": v}))
	}
	for _, v := range []string{"7", "7", "3", "3", "3", "3", "5", "1", "9", "2"} {
		b = append(b, obs(map[string]string{"value": v}))
	}
	got := p.Compare([]CellPair{{
		Key: map[string]string{"question_id": "q1", "context_bucket": "0"}, A: a, B: b,
	}}, 0.15, 0)
	if len(got.Cells) != 1 {
		t.Fatalf("cells = %d, want 1", len(got.Cells))
	}
	extra := got.Cells[0].Extra
	aDist, ok := extra["a_dist"].(map[string]int)
	if !ok {
		t.Fatalf("Extra[a_dist] = %#v, want map[string]int", extra["a_dist"])
	}
	bDist, ok := extra["b_dist"].(map[string]int)
	if !ok {
		t.Fatalf("Extra[b_dist] = %#v, want map[string]int", extra["b_dist"])
	}
	if aDist["7"] != 3 || aDist["3"] != 2 {
		t.Errorf("a_dist = %v, want 7×3, 3×2", aDist)
	}
	if bDist["3"] != 4 || bDist["7"] != 2 {
		t.Errorf("b_dist = %v, want 3×4, 7×2", bDist)
	}
	// 判定语义不变：JSD 仍算、verdict 仍由均值与阈值决定
	if got.Statistic == nil || got.Verdict == "" {
		t.Errorf("statistic/verdict 不应受 Extra 扩展影响: %+v", got)
	}
}

func TestToolcallCellCarriesDistributions(t *testing.T) {
	p, _ := Get("toolcall")
	var a, b []toolcallObs
	for i := 0; i < 10; i++ {
		a = append(a, tcObs("get_weather", true, 1))
		b = append(b, tcObs("get_weather", true, 1))
	}
	for i := 0; i < 5; i++ {
		b = append(b, tcObs("unit_convert", true, 1))
	}
	cells := []CellPair{tcCell(
		map[string]string{"question_id": "q1", "context_bucket": "0"}, a, b)}
	got := p.Compare(cells, 0.05, 0)
	if len(got.Cells) != 1 {
		t.Fatalf("cells = %d, want 1", len(got.Cells))
	}
	extra := got.Cells[0].Extra
	aDist, ok := extra["a_dist"].(map[string]int)
	if !ok {
		t.Fatalf("Extra[a_dist] = %#v, want map[string]int", extra["a_dist"])
	}
	bDist, ok := extra["b_dist"].(map[string]int)
	if !ok {
		t.Fatalf("Extra[b_dist] = %#v, want map[string]int", extra["b_dist"])
	}
	if aDist["get_weather"] != 10 || len(aDist) != 1 {
		t.Errorf("a_dist = %v, want get_weather×10", aDist)
	}
	if bDist["get_weather"] != 10 || bDist["unit_convert"] != 5 {
		t.Errorf("b_dist = %v, want get_weather×10 + unit_convert×5", bDist)
	}
}

func TestThinkEffortCellCarriesHistogram(t *testing.T) {
	p, _ := Get("think-effort")
	a := []int{512, 480, 530, 495, 510, 520, 490, 505, 515, 500}
	// B 侧思考量被砍半：直方图应能看出两侧分离
	b := []int{256, 240, 265, 248, 255, 260, 245, 252, 258, 250}
	cells := []CellPair{teCell(
		map[string]string{"question_id": "q1", "context_bucket": "0",
			"thinking_effort": "high", "protocol": "openai-chat"}, "tokens", a, b)}
	got := p.Compare(cells, 0.01, 0)
	if len(got.Cells) != 1 {
		t.Fatalf("cells = %d, want 1", len(got.Cells))
	}
	h, ok := got.Cells[0].Extra["hist"].(stats.Hist)
	if !ok {
		t.Fatalf("Extra[hist] = %#v, want stats.Hist", got.Cells[0].Extra["hist"])
	}
	if h.Empty() {
		t.Fatal("直方图不应为空")
	}
	if sum(h.A) != 10 || sum(h.B) != 10 {
		t.Errorf("counts 总和 = %d/%d, want 10/10", sum(h.A), sum(h.B))
	}
	// 两侧完全分离：A 全在高端 bins、B 全在低端 bins，无重叠 bin
	for i := range h.A {
		if h.A[i] > 0 && h.B[i] > 0 {
			t.Errorf("bin %d 两侧重叠 (%d/%d)，与分离的样本矛盾", i, h.A[i], h.B[i])
		}
	}
}

func sum(xs []int) int {
	s := 0
	for _, x := range xs {
		s += x
	}
	return s
}
