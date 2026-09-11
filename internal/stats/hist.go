package stats

// hist.go：两侧样本的联合直方图。
// 描述性数据（不参与判定）：报告层用它渲染传输指标、思考量等
// 连续量的分布对比，让 pass/fail 之外还有形状层面的证据。

// Hist 联合直方图：A/B 两侧共享同一组分箱边界，counts 逐 bin 对齐。
// Edges 长度 = bins+1；Empty() 为真时 Edges/A/B 均为 nil。
type Hist struct {
	Edges []float64 `json:"edges"`
	A     []int     `json:"a"`
	B     []int     `json:"b"`
}

// Empty 无任何样本（两侧都空）。
func (h Hist) Empty() bool {
	return len(h.A) == 0 && len(h.B) == 0
}

// JointHist 把两侧样本分进 bins 个等宽 bin，边界由两侧合并的
// [min, max] 决定（min==max 时上界 +1，避免除零）。
// 值落 bin 规则：x ∈ [Edges[i], Edges[i+1])，最大值闭进最后一个 bin。
// bins < 1 时按 1 处理；两侧都空返回零值 Hist。
func JointHist(a, b []float64, bins int) Hist {
	if bins < 1 {
		bins = 1
	}
	if len(a) == 0 && len(b) == 0 {
		return Hist{}
	}
	lo, hi := a[0], a[0]
	for _, x := range a {
		if x < lo {
			lo = x
		}
		if x > hi {
			hi = x
		}
	}
	for _, x := range b {
		if x < lo {
			lo = x
		}
		if x > hi {
			hi = x
		}
	}
	if hi == lo {
		hi = lo + 1
	}

	edges := make([]float64, bins+1)
	for i := range edges {
		edges[i] = lo + (hi-lo)*float64(i)/float64(bins)
	}
	edges[bins] = hi // 消除浮点累计误差，保证上界精确

	h := Hist{Edges: edges, A: make([]int, bins), B: make([]int, bins)}
	for _, x := range a {
		h.A[binIndex(x, edges)]++
	}
	for _, x := range b {
		h.B[binIndex(x, edges)]++
	}
	return h
}

// binIndex 值 → bin 下标；越界值夹到两端 bin，最大值归最后一个 bin。
func binIndex(x float64, edges []float64) int {
	n := len(edges) - 1
	// 线性扫描足够：bins 是报告尺度的小常数（<100）
	for i := 0; i < n; i++ {
		if x < edges[i+1] || i == n-1 {
			return i
		}
	}
	return n - 1
}
