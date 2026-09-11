package stats

import "testing"

func TestJointHistEmpty(t *testing.T) {
	// 两侧都空 → 零值 Hist（Edges 为 nil），调用方以 Empty() 判别
	h := JointHist(nil, nil, 8)
	if !h.Empty() {
		t.Errorf("JointHist(nil,nil) = %+v, want empty", h)
	}
	if h.Edges != nil || h.A != nil || h.B != nil {
		t.Errorf("空直方图字段应为 nil: %+v", h)
	}
}

func TestJointHistConstantValues(t *testing.T) {
	// 所有样本同值（hi==lo）→ 上界扩 1，全部落进第一个 bin
	h := JointHist([]float64{5, 5, 5}, []float64{5}, 4)
	if h.Empty() {
		t.Fatal("不应为空")
	}
	if len(h.Edges) != 5 {
		t.Fatalf("edges = %v, want 5 个边界", h.Edges)
	}
	if h.A[0] != 3 || h.B[0] != 1 {
		t.Errorf("counts = %v / %v, want 全在 bin 0", h.A, h.B)
	}
	for i := 1; i < 4; i++ {
		if h.A[i] != 0 || h.B[i] != 0 {
			t.Errorf("bin %d 不应有样本: %v / %v", i, h.A, h.B)
		}
	}
}

func TestJointHistSeparation(t *testing.T) {
	// A 全在低端、B 全在高端 → 各自落入两端 bin，边界覆盖 [0,10]
	a := []float64{0, 0.5, 1, 1.5}
	b := []float64{8.5, 9, 9.5, 10}
	h := JointHist(a, b, 5)
	if h.Edges[0] != 0 || h.Edges[5] != 10 {
		t.Errorf("edges = %v, want [0,10]", h.Edges)
	}
	// bin 宽 2：a 全在 bin 0，b 全在 bin 4（上边界闭到最后一个 bin）
	if sum(h.A[1:]) != 0 || h.A[0] != 4 {
		t.Errorf("A counts = %v, want 全在 bin 0", h.A)
	}
	if sum(h.B[:4]) != 0 || h.B[4] != 4 {
		t.Errorf("B counts = %v, want 全在 bin 4", h.B)
	}
}

func TestJointHistCountsAndMonotonicEdges(t *testing.T) {
	a := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	b := []float64{2, 4, 6, 8}
	h := JointHist(a, b, 6)
	if sum(h.A) != len(a) || sum(h.B) != len(b) {
		t.Errorf("counts 总和 = %d/%d, want %d/%d", sum(h.A), sum(h.B), len(a), len(b))
	}
	for i := 1; i < len(h.Edges); i++ {
		if h.Edges[i] <= h.Edges[i-1] {
			t.Fatalf("edges 必须严格递增: %v", h.Edges)
		}
	}
	// bins < 1 时按 1 处理
	h1 := JointHist(a, b, 0)
	if len(h1.Edges) != 2 || sum(h1.A) != len(a) {
		t.Errorf("bins=0 应回退单 bin: %+v", h1)
	}
}

func sum(xs []int) int {
	s := 0
	for _, x := range xs {
		s += x
	}
	return s
}
