package stats

import (
	"math"
	"testing"
)

func almostEqual(a, b, tol float64) bool {
	return math.Abs(a-b) <= tol
}

func TestJSD(t *testing.T) {
	// 同一分布 JSD = 0
	same := Dist{"7": 5, "3": 5}
	if got := JSD(same, same); got != 0 {
		t.Errorf("JSD(same,same) = %v, want 0", got)
	}
	// 不相交支撑 JSD = 1（base 2）
	if got := JSD(Dist{"a": 1}, Dist{"b": 1}); !almostEqual(got, 1, 1e-12) {
		t.Errorf("JSD(disjoint) = %v, want 1", got)
	}
	// 手算例：p=(1,0)，q=(0.5,0.5)，m=(0.75,0.25)
	// JSD = 0.5·1·log2(1/0.75) + 0.5·[0.5·log2(0.5/0.75) + 0.5·log2(0.5/0.25)]
	got := JSD(Dist{"a": 1}, Dist{"a": 1, "b": 1})
	want := 0.5*math.Log2(1/0.75) + 0.5*(0.5*math.Log2(0.5/0.75)+0.5*math.Log2(2))
	if !almostEqual(got, want, 1e-9) {
		t.Errorf("JSD = %v, want %v", got, want)
	}
	// 空分布返回 1
	if got := JSD(Dist{}, Dist{"a": 1}); got != 1 {
		t.Errorf("JSD(empty,·) = %v, want 1", got)
	}
}

func TestChi2SF(t *testing.T) {
	// 对照 R: pchisq(q, df, lower.tail=FALSE)
	cases := []struct {
		x    float64
		df   int
		want float64
	}{
		{3.841, 1, 0.05},
		{6.635, 1, 0.01},
		{5.991, 2, 0.05},
		{9.210, 2, 0.01},
		{11.070, 5, 0.05},
		{15.086, 5, 0.01},
		{0, 1, 1},
	}
	for _, c := range cases {
		if got := chi2SF(c.x, c.df); !almostEqual(got, c.want, 5e-4) {
			t.Errorf("chi2SF(%v,%v) = %v, want %v", c.x, c.df, got, c.want)
		}
	}
}

func TestFisherExact2x2(t *testing.T) {
	// 对照 R fisher.test 的已知结果
	// [[1,9],[11,3]] → p ≈ 0.00276（经典茶味测验变体）
	if got := fisherExact2x2(1, 9, 11, 3); !almostEqual(got, 0.002759, 1e-4) {
		t.Errorf("fisher(1,9,11,3) = %v, want ≈0.002759", got)
	}
	// 对称表 [[5,5],[5,5]] → p = 1
	if got := fisherExact2x2(5, 5, 5, 5); !almostEqual(got, 1, 1e-9) {
		t.Errorf("fisher(5,5,5,5) = %v, want 1", got)
	}
	// 极端表 [[0,10],[10,0]] → p = 2/C(20,10) ≈ 1.08e-5
	if got := fisherExact2x2(0, 10, 10, 0); !almostEqual(got, 1.0825e-5, 1e-7) {
		t.Errorf("fisher(0,10,10,0) = %v, want ≈1.08e-5", got)
	}
}

func TestChi2Homogeneity(t *testing.T) {
	// 大样本走卡方：两组完全相同分布 → p = 1
	p, method := Chi2Homogeneity([]int{50, 30, 20}, []int{50, 30, 20}, 5)
	if method != "chi2" || !almostEqual(p, 1, 1e-9) {
		t.Errorf("identical dists: p=%v method=%v, want p=1 chi2", p, method)
	}
	// 明显不同的大样本分布 → p 很小
	p, _ = Chi2Homogeneity([]int{90, 10}, []int{10, 90}, 5)
	if p > 1e-10 {
		t.Errorf("very different dists: p=%v, want ≈0", p)
	}
	// 小样本 2×2 回退 Fisher：[[0,10],[10,0]] 期望频数 5<minExpected? 列和 10，
	// eA=10·10/20=5 ≥ 5 → 走卡方。把 minExpected 调大逼 Fisher。
	p, method = Chi2Homogeneity([]int{0, 10}, []int{10, 0}, 6)
	if method != "fisher" {
		t.Errorf("method = %v, want fisher", method)
	}
	// Fisher 与卡方在极端表上都给很小的 p
	if p > 1e-4 {
		t.Errorf("fisher path p=%v, want small", p)
	}
	// tokenizer 场景：[[match, mismatch],[total, 0]]，total=10 全不等 → 小 p
	p, _ = Chi2Homogeneity([]int{0, 10}, []int{10, 0}, 5)
	if p > 0.01 {
		t.Errorf("tokenizer all-mismatch: p=%v, want <0.01", p)
	}
	// 非法输入
	if p, method = Chi2Homogeneity([]int{1}, []int{1, 2}, 5); method != "invalid" || !math.IsNaN(p) {
		t.Errorf("mismatched lengths: p=%v method=%v", p, method)
	}
}

func TestQuantile(t *testing.T) {
	xs := []float64{1, 2, 3, 4, 5}
	if got := Quantile(xs, 0.5); got != 3 {
		t.Errorf("median = %v, want 3", got)
	}
	if got := Quantile(xs, 0); got != 1 {
		t.Errorf("q0 = %v, want 1", got)
	}
	if got := Quantile(xs, 1); got != 5 {
		t.Errorf("q1 = %v, want 5", got)
	}
	if !math.IsNaN(Quantile(nil, 0.5)) {
		t.Error("empty quantile should be NaN")
	}
	// 不修改原切片
	if xs[0] != 1 {
		t.Error("Quantile mutated input")
	}
}

func TestMean(t *testing.T) {
	if got := Mean([]float64{1, 2, 6}); got != 3 {
		t.Errorf("Mean = %v, want 3", got)
	}
	if !math.IsNaN(Mean(nil)) {
		t.Error("empty mean should be NaN")
	}
}
