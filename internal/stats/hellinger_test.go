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

func TestUnbiasedCosineDistHandComputed(t *testing.T) {
	// 1. 单序列（n=1）退化为标准余弦距离：u=[1, 0], v=[0, 1] → cos=0, d=1
	if got := unbiasedCosineDist([]float64{1, 0}, 1, 1, []float64{0, 1}, 1, 1); !almostEqual(got, 1.0, 1e-12) {
		t.Fatalf("disjoint n=1 got %v, want 1", got)
	}
	// 完全相同向量严格返回 0（无 1-cos 浮点相消残差）
	if got := unbiasedCosineDist([]float64{0.3, 0.7}, 0.58, 1, []float64{0.3, 0.7}, 0.58, 1); got != 0 {
		t.Fatalf("identical n=1 got %v, want 0", got)
	}

	// 2. 多序列手算 U-统计量：
	// A 由两条响应构成：x1=[0.8, 0.2], x2=[0.6, 0.4] → sumA=[1.4, 0.6], normSqA=0.68+0.52=1.20
	//   aa = (1.4^2 + 0.6^2 - 1.20) / (2*1) = (2.32 - 1.20) / 2 = 0.56
	// B 由两条响应构成：y1=[0.2, 0.8], y2=[0.4, 0.6] → sumB=[0.6, 1.4], normSqB=1.20
	//   bb = 0.56
	//   ab = (1.4*0.6 + 0.6*1.4) / 4 = 1.68 / 4 = 0.42
	//   cos_u = 0.42 / 0.56 = 0.75 → d = sqrt(1 - 0.75) = 0.5
	got := unbiasedCosineDist([]float64{1.4, 0.6}, 1.20, 2, []float64{0.6, 1.4}, 1.20, 2)
	if !almostEqual(got, 0.5, 1e-12) {
		t.Fatalf("hand-computed U-Cos n=2 got %.15f, want 0.5", got)
	}

	// 两侧含相同多响应均值且完全一致：严格返回 0
	if gotSame := unbiasedCosineDist([]float64{1.4, 0.6}, 1.20, 2, []float64{1.4, 0.6}, 1.20, 2); gotSame != 0 {
		t.Fatalf("identical n=2 got %v, want 0", gotSame)
	}

	// 非法输入返回 NaN
	if v := unbiasedCosineDist(nil, 0, 1, nil, 0, 1); !math.IsNaN(v) {
		t.Errorf("empty got %v, want NaN", v)
	}
	if v := unbiasedCosineDist([]float64{1}, 1, 0, []float64{1}, 1, 1); !math.IsNaN(v) {
		t.Errorf("zero nA got %v, want NaN", v)
	}
}

func TestNumRandomProfileSubspaceSensitivity(t *testing.T) {
	// 构造两条边际值集合完全相同、末位数字完全相同，但前后位置顺序翻转的序列：
	// 前半全是小数（1..40），后半全是大数（316..355） vs 反转。
	// 验证：ValueD == 0、LastDigitD == 0，但 PrefixD > 0.5、PositionD > 0.5 驱动 Composite > 0。
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
	if d.ValueD != 0 {
		t.Errorf("边际集合完全相同时 ValueD = %v, want 0", d.ValueD)
	}
	if d.LastDigitD != 0 {
		t.Errorf("末位分布完全相同时 LastDigitD = %v, want 0", d.LastDigitD)
	}
	if d.PositionD <= 0.5 {
		t.Errorf("前后值域完全对调时 PositionD = %v, 应显著大于 0.5", d.PositionD)
	}
	if d.PrefixD <= 0.5 {
		t.Errorf("开篇值域完全对调时 PrefixD = %v, 应显著大于 0.5", d.PrefixD)
	}
	wantComp := math.Sqrt(
		numRandomWeightValue*d.ValueD*d.ValueD +
			numRandomWeightPrefix*d.PrefixD*d.PrefixD +
			numRandomWeightTransition*d.TransitionD*d.TransitionD +
			numRandomWeightPosition*d.PositionD*d.PositionD +
			numRandomWeightLastDigit*d.LastDigitD*d.LastDigitD,
	)
	if !almostEqual(d.Composite, wantComp, 1e-12) {
		t.Errorf("Composite = %v, want %v", d.Composite, wantComp)
	}

	// 验证 Merge 顺序无关性与结合律（模拟并发采集记录乱序落盘）
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

func TestNumRandomProfileTransitionOrderSensitivity(t *testing.T) {
	// 构造两条边际值、末位数字完全相同，4 分块与开篇权重也几乎相同，仅相邻跃迁顺序不同的序列：
	// seqAlt  = [10, 340, 10, 340, ...]（100% 跨区间反相关跃迁）
	// seqPair = [10, 10, 340, 340, ...]（50% 同区间停留 + 50% 跨区间跃迁）
	seqAlt := make([]int, 300)
	seqPair := make([]int, 300)
	for i := 0; i < 300; i++ {
		if i%2 == 0 {
			seqAlt[i] = 10
		} else {
			seqAlt[i] = 340
		}
		if (i/2)%2 == 0 {
			seqPair[i] = 10
		} else {
			seqPair[i] = 340
		}
	}

	pa := NewNumRandomProfile(1, 355)
	pb := NewNumRandomProfile(1, 355)
	pa.AddSequence(seqAlt)
	pa.AddSequence(seqAlt)
	pb.AddSequence(seqPair)
	pb.AddSequence(seqPair)

	d, ok := CompareNumRandomProfiles(pa, pb)
	if !ok {
		t.Fatal("CompareNumRandomProfiles 应成功")
	}
	if d.ValueD != 0 || d.LastDigitD != 0 {
		t.Errorf("同多重集下 ValueD=%v LastDigitD=%v, want 0", d.ValueD, d.LastDigitD)
	}
	if d.TransitionD <= 0.5 {
		t.Errorf("相邻转移结构剧烈改变时 TransitionD = %v, 应大于 0.5", d.TransitionD)
	}
	if d.Composite <= 0.15 {
		t.Errorf("TransitionD 应有效驱动 Composite=%v > 0.15", d.Composite)
	}
}
