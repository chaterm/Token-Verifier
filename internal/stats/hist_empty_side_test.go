package stats

import "testing"

func TestJointHistSingleSideEmpty(t *testing.T) {
	// A 为空、B 非空：Split-Half 中 repeats=1 探针的典型情形（gen 为空、imp 非空）。
	h := JointHist(nil, []float64{0.1, 0.2, 0.3}, 3)
	if h.Empty() {
		t.Fatal("B 非空时直方图不应为空")
	}
	totalB := 0
	for _, c := range h.B {
		totalB += c
	}
	if totalB != 3 {
		t.Fatalf("B 侧计数 = %d, want 3", totalB)
	}
	for i, c := range h.A {
		if c != 0 {
			t.Fatalf("A 为空时第 %d 个 bin 应为 0, got %d", i, c)
		}
	}

	// A 非空、B 为空
	h2 := JointHist([]float64{1.0, 2.0}, nil, 4)
	totalA := 0
	for _, c := range h2.A {
		totalA += c
	}
	if totalA != 2 {
		t.Fatalf("A 侧计数 = %d, want 2", totalA)
	}

	// 两侧都空仍返回零值
	if h3 := JointHist(nil, nil, 4); !h3.Empty() {
		t.Fatal("两侧都空应返回空直方图")
	}
}
