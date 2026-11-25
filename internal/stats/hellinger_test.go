package stats

import (
	"math"
	"testing"
)

func TestHellingerSmoothedBasic(t *testing.T) {
	// 完全相同的非零计数：距离严格为 0（无浮点相消残差）
	c := []int{0, 12, 0, 5, 1, 0, 30}
	if got := HellingerSmoothed(c, c, DefaultHellingerAlpha); got != 0 {
		t.Fatalf("HellingerSmoothed(same, same) = %v, want 0", got)
	}

	// 手算 2 桶例：a=[2, 0] (N=2), b=[0, 2] (N=2), alpha=0.5, K=2
	// p = [(2+0.5)/(2+1), (0+0.5)/(2+1)] = [5/6, 1/6]
	// q = [1/6, 5/6]
	// H = sqrt( (sqrt(5/6)-sqrt(1/6))^2 ) = sqrt(5/6) - sqrt(1/6)
	got := HellingerSmoothed([]int{2, 0}, []int{0, 2}, 0.5)
	want := math.Sqrt(5.0/6.0) - math.Sqrt(1.0/6.0)
	if !almostEqual(got, want, 1e-12) {
		t.Fatalf("HellingerSmoothed([2,0],[0,2],0.5) = %.15f, want %.15f", got, want)
	}

	// alpha=0 退化为标准无平滑 Hellinger：完全不交支撑时距离为 1
	if got0 := HellingerSmoothed([]int{10, 0}, []int{0, 10}, 0); !almostEqual(got0, 1.0, 1e-12) {
		t.Fatalf("HellingerSmoothed disjoint alpha=0 = %v, want 1", got0)
	}

	// 非法输入：空向量、维度不等、零总计数、负计数、负 alpha 均返回 NaN
	invalidCases := []struct {
		name  string
		a, b  []int
		alpha float64
	}{
		{"empty", nil, nil, 0.5},
		{"len_mismatch", []int{1, 2}, []int{1, 2, 3}, 0.5},
		{"zero_total_a", []int{0, 0}, []int{1, 2}, 0.5},
		{"zero_total_b", []int{1, 2}, []int{0, 0}, 0.5},
		{"negative_count", []int{-1, 5}, []int{1, 2}, 0.5},
		{"negative_alpha", []int{1, 2}, []int{1, 2}, -0.1},
	}
	for _, tc := range invalidCases {
		if v := HellingerSmoothed(tc.a, tc.b, tc.alpha); !math.IsNaN(v) {
			t.Errorf("%s: got %v, want NaN", tc.name, v)
		}
	}
}

func TestNumRandomProfileSubspaceSensitivity(t *testing.T) {
	// 构造两条边际值集合完全相同、末位数字完全相同，但前后位置顺序翻转的序列：
	// 前半全是小数（1..40），后半全是大数（316..355） vs 反转。
	// 验证：ValueH == 0、LastDigitH == 0，但 PositionH > 0 驱动 Composite > 0。
	seqAsc := make([]int, 0, 160)
	for rep := 0; rep < 2; rep++ {
		for v := 1; v <= 40; v++ {
			seqAsc = append(seqAsc, v)
		}
	}
	for rep := 0; rep < 2; rep++ {
		for v := 316; v <= 355; v++ {
			seqAsc = append(seqAsc, v)
		}
	}
	seqDesc := make([]int, len(seqAsc))
	for i := range seqAsc {
		seqDesc[i] = seqAsc[len(seqAsc)-1-i]
	}

	pa := NewNumRandomProfile(1, 355)
	pb := NewNumRandomProfile(1, 355)
	if !pa.AddSequence(seqAsc) || !pb.AddSequence(seqDesc) {
		t.Fatal("AddSequence 应成功")
	}

	d, ok := CompareNumRandomProfiles(pa, pb)
	if !ok {
		t.Fatal("CompareNumRandomProfiles 应成功")
	}
	if d.ValueH != 0 {
		t.Errorf("边际集合完全相同时 ValueH = %v, want 0", d.ValueH)
	}
	if d.LastDigitH != 0 {
		t.Errorf("末位分布完全相同时 LastDigitH = %v, want 0", d.LastDigitH)
	}
	if d.PositionH <= 0.5 {
		t.Errorf("前后值域完全对调时 PositionH = %v, 应显著大于 0.5", d.PositionH)
	}
	wantComp := math.Sqrt(0.125 * d.PositionH * d.PositionH)
	if !almostEqual(d.Composite, wantComp, 1e-12) {
		t.Errorf("Composite = %v, want %v", d.Composite, wantComp)
	}

	// 验证 Merge 顺序无关性（模拟并发采集记录乱序落盘）
	m1 := NewNumRandomProfile(1, 355)
	m1.AddSequence(seqAsc)
	m1.AddSequence(seqDesc)

	m2 := NewNumRandomProfile(1, 355)
	pAsc := NewNumRandomProfile(1, 355)
	pDesc := NewNumRandomProfile(1, 355)
	pDesc.AddSequence(seqDesc)
	pAsc.AddSequence(seqAsc)
	m2.Merge(pDesc)
	m2.Merge(pAsc)

	dSame, ok := CompareNumRandomProfiles(m1, m2)
	if !ok || dSame.Composite != 0 {
		t.Fatalf("乱序池化结果应严格一致: ok=%v, d=%+v", ok, dSame)
	}
}
