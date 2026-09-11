package stats

import (
	"math"
	"testing"
)

func TestMannWhitneyUSameDistribution(t *testing.T) {
	a := []float64{1, 2, 3, 4, 5, 6, 7, 8}
	b := []float64{1.5, 2.5, 3.5, 4.5, 5.5, 6.5, 7.5, 8.5}
	p := MannWhitneyU(a, b)
	if p < 0.5 {
		t.Errorf("交错分布 p = %v，应接近 1（无差异）", p)
	}
}

func TestMannWhitneyUDisjointDistribution(t *testing.T) {
	// 完全分离的两组：n=10/10 时精确 p≈1e-5，正态近似也应远小于 0.01
	a := []float64{10, 11, 12, 13, 14, 15, 16, 17, 18, 19}
	b := []float64{100, 101, 102, 103, 104, 105, 106, 107, 108, 109}
	p := MannWhitneyU(a, b)
	if p > 0.001 {
		t.Errorf("完全分离 p = %v，应远小于 0.001", p)
	}
	// 对称性：交换两侧 p 值不变
	if p2 := MannWhitneyU(b, a); math.Abs(p-p2) > 1e-12 {
		t.Errorf("交换两侧 p 不一致: %v vs %v", p, p2)
	}
}

func TestMannWhitneyUTies(t *testing.T) {
	// 全部同值：无法区分，p=1
	a := []float64{5, 5, 5, 5}
	b := []float64{5, 5, 5, 5}
	if p := MannWhitneyU(a, b); p != 1 {
		t.Errorf("全同值 p = %v, want 1", p)
	}
	// 大量并列 + 分离趋势：仍能判出显著差异（方差修正生效）
	c := []float64{1, 1, 1, 1, 1, 2, 2, 2, 2, 2}
	d := []float64{9, 9, 9, 9, 9, 8, 8, 8, 8, 8}
	if p := MannWhitneyU(c, d); p > 0.01 {
		t.Errorf("并列分离组 p = %v，应 < 0.01", p)
	}
}

func TestMannWhitneyUEmpty(t *testing.T) {
	if p := MannWhitneyU(nil, []float64{1}); !math.IsNaN(p) {
		t.Errorf("空组 p = %v, want NaN", p)
	}
}

func TestFisherCombine(t *testing.T) {
	// 全部 p=1 → 合并 p=1
	if p := FisherCombine([]float64{1, 1, 1}); math.Abs(p-1) > 1e-9 {
		t.Errorf("全 1 合并 = %v, want 1", p)
	}
	// 强证据合并：0.001 × 3 → -2Σln = 41.4，df=6，p 应极小
	if p := FisherCombine([]float64{0.001, 0.001, 0.001}); p > 1e-6 {
		t.Errorf("强证据合并 = %v，应 < 1e-6", p)
	}
	// 单个 p 值：合并后应接近原值（df=2 时 -2ln p 的生存函数 = p）
	for _, pv := range []float64{0.01, 0.05, 0.5} {
		got := FisherCombine([]float64{pv})
		if math.Abs(got-pv) > 1e-9 {
			t.Errorf("单值合并 %v = %v, want %v", pv, got, pv)
		}
	}
	// p=0 顶替为最小正浮点，不 panic 不 NaN
	if p := FisherCombine([]float64{0, 0.5}); math.IsNaN(p) || p > 1e-100 {
		t.Errorf("含 0 合并 = %v，应为极小正数", p)
	}
	if p := FisherCombine(nil); !math.IsNaN(p) {
		t.Errorf("空输入 = %v, want NaN", p)
	}
}
