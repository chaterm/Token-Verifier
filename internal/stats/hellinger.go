package stats

import "math"

// DefaultHellingerAlpha ModelTrace 使用的 Jeffreys 先验平滑常数（Dir(0.5)），
// 消除高维稀疏计数桶（如 355 维值分布）在有限样本下的零桶噪声膨胀。
const DefaultHellingerAlpha = 0.5

const (
	numRandomChunks    = 4
	numRandomRangeBins = 16
	numRandomDigits    = 10
)

// HellingerSmoothed 计算两个等长非负整数计数向量的平滑 Hellinger 距离（值域 [0, 1]）。
//
// 采用欧氏平方差定义 H = sqrt( 0.5 * sum_i (sqrt(p_i) - sqrt(q_i))^2 )，
// 避免 1 - BC 在 p ≈ q 时发生 IEEE754 灾难性相消，保证相同计数严格返回 0。
// 输入维度为空、长度不等、存在负计数、任一侧原始总计数 ≤ 0 或 alpha < 0 时返回 NaN。
func HellingerSmoothed(a, b []int, alpha float64) float64 {
	if len(a) == 0 || len(a) != len(b) || alpha < 0 || math.IsNaN(alpha) || math.IsInf(alpha, 0) {
		return math.NaN()
	}
	totalA, totalB := 0, 0
	for i := range a {
		if a[i] < 0 || b[i] < 0 {
			return math.NaN()
		}
		totalA += a[i]
		totalB += b[i]
	}
	if totalA <= 0 || totalB <= 0 {
		return math.NaN()
	}

	k := float64(len(a))
	denomA := float64(totalA) + alpha*k
	denomB := float64(totalB) + alpha*k
	if denomA <= 0 || denomB <= 0 {
		return math.NaN()
	}

	var sumSq float64
	for i := range a {
		pa := (float64(a[i]) + alpha) / denomA
		pb := (float64(b[i]) + alpha) / denomB
		diff := math.Sqrt(pa) - math.Sqrt(pb)
		sumSq += diff * diff
	}
	h := math.Sqrt(0.5 * sumSq)
	if h > 1 {
		return 1
	}
	return h
}

// NumRandomProfile 数字随机序列探针的三子空间特征累积器（单格与跨格池化同构复用）：
//  1. Values:     [MinVal, MaxVal] 边际值计数（默认 355 维）
//  2. PosChunks:  序列按索引 4 等分 × 16 个均匀值域桶
//  3. LastDigits: 末位数字 v % 10 计数（10 维）
type NumRandomProfile struct {
	MinVal     int
	MaxVal     int
	Values     []int
	PosChunks  [numRandomChunks][numRandomRangeBins]int
	LastDigits [numRandomDigits]int
	TotalInts  int
	SeqCount   int
}

// NumRandomDistance 三子空间平滑 Hellinger 分量及加权几何组合距离。
type NumRandomDistance struct {
	Composite  float64
	ValueH     float64
	PositionH  float64
	LastDigitH float64
}

// NewNumRandomProfile 创建给定闭区间 [minVal, maxVal] 的特征累积器。
func NewNumRandomProfile(minVal, maxVal int) *NumRandomProfile {
	width := maxVal - minVal + 1
	if width < 1 {
		width = 1
	}
	return &NumRandomProfile{
		MinVal: minVal,
		MaxVal: maxVal,
		Values: make([]int, width),
	}
}

// AddSequence 向累积器追加一条有效整数序列（长度须 ≥ 4 且所有元素落在 [MinVal, MaxVal]）。
func (p *NumRandomProfile) AddSequence(seq []int) bool {
	if p == nil || len(seq) < numRandomChunks || p.MaxVal < p.MinVal {
		return false
	}
	width := p.MaxVal - p.MinVal + 1
	if len(p.Values) != width {
		return false
	}
	for _, v := range seq {
		if v < p.MinVal || v > p.MaxVal {
			return false
		}
	}

	n := len(seq)
	q, r := n/numRandomChunks, n%numRandomChunks
	offset := 0
	for s := 0; s < numRandomChunks; s++ {
		chunkLen := q
		if s < r {
			chunkLen++
		}
		end := offset + chunkLen
		for _, v := range seq[offset:end] {
			p.Values[v-p.MinVal]++
			bin := (v - p.MinVal) * numRandomRangeBins / width
			if bin >= numRandomRangeBins {
				bin = numRandomRangeBins - 1
			}
			p.PosChunks[s][bin]++
			p.LastDigits[v%numRandomDigits]++
		}
		offset = end
	}
	p.TotalInts += n
	p.SeqCount++
	return true
}

// Merge 将同值域的另一累积器计数合并到当前累积器（满足交换律与结合律）。
func (p *NumRandomProfile) Merge(other *NumRandomProfile) bool {
	if p == nil || other == nil || p.MinVal != other.MinVal || p.MaxVal != other.MaxVal || len(p.Values) != len(other.Values) {
		return false
	}
	for i := range p.Values {
		p.Values[i] += other.Values[i]
	}
	for s := 0; s < numRandomChunks; s++ {
		for b := 0; b < numRandomRangeBins; b++ {
			p.PosChunks[s][b] += other.PosChunks[s][b]
		}
	}
	for d := 0; d < numRandomDigits; d++ {
		p.LastDigits[d] += other.LastDigits[d]
	}
	p.TotalInts += other.TotalInts
	p.SeqCount += other.SeqCount
	return true
}

// CompareNumRandomProfiles 计算两侧累积器的 Jeffreys 平滑 Hellinger 组合距离（0.75 : 0.125 : 0.125）。
func CompareNumRandomProfiles(a, b *NumRandomProfile) (NumRandomDistance, bool) {
	if a == nil || b == nil || a.MinVal != b.MinVal || a.MaxVal != b.MaxVal || a.TotalInts <= 0 || b.TotalInts <= 0 {
		return NumRandomDistance{}, false
	}
	valH := HellingerSmoothed(a.Values, b.Values, DefaultHellingerAlpha)
	if math.IsNaN(valH) {
		return NumRandomDistance{}, false
	}
	var posSqSum float64
	for s := 0; s < numRandomChunks; s++ {
		ch := HellingerSmoothed(a.PosChunks[s][:], b.PosChunks[s][:], DefaultHellingerAlpha)
		if math.IsNaN(ch) {
			return NumRandomDistance{}, false
		}
		posSqSum += ch * ch
	}
	posH := math.Sqrt(posSqSum / float64(numRandomChunks))
	digH := HellingerSmoothed(a.LastDigits[:], b.LastDigits[:], DefaultHellingerAlpha)
	if math.IsNaN(digH) {
		return NumRandomDistance{}, false
	}

	comp := math.Sqrt(0.75*valH*valH + 0.125*posH*posH + 0.125*digH*digH)
	if comp > 1 {
		comp = 1
	}
	return NumRandomDistance{
		Composite:  comp,
		ValueH:     valH,
		PositionH:  posH,
		LastDigitH: digH,
	}, true
}
