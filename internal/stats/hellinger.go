package stats

import "math"

// DefaultHellingerAlpha ModelTrace 使用的 Jeffreys 先验平滑常数（Dir(0.5)），
// 消除高维稀疏计数桶（如 355 维值分布）在有限样本下的零桶噪声膨胀。
const DefaultHellingerAlpha = 0.5

const (
	numRandomChunks      = 4
	numRandomRangeBins   = 16
	numRandomDigits      = 10
	numRandomTransBins   = 8
	numRandomTransCells  = numRandomTransBins * numRandomTransBins
	numRandomPrefixDecay = 5.0 // 相对位置指数衰减 w_i = exp(-5*i/L)，聚焦开篇约 20% 序列的无条件先验指纹

	// 2026-11-26 跨 6 模型家族 18 run（153 对配对：GPT-6-luna / GPT-5.6-luna / DeepSeek / GLM / Qwen / Kimi / Hy）
	// 响应级无偏余弦距离（U-Cos）标定权重：15/15 同模型 ≤ 0.081，138/138 跨模型 ≥ 0.145（AUC=1.0000，0 漏报）。
	numRandomWeightValue      = 0.45
	numRandomWeightPrefix     = 0.20
	numRandomWeightTransition = 0.15
	numRandomWeightPosition   = 0.10
	numRandomWeightLastDigit  = 0.10
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

// NumRandomProfile 数字随机序列探针的五子空间响应级无偏余弦（U-Cos）特征累积器（单格与跨格池化同构复用）：
//  1. Value:      [MinVal, MaxVal] 全序列边际值分布（默认 355 维）
//  2. Prefix:     相对位置指数衰减 w_i = exp(-5*i/L) 开篇先验值分布（同 W 维，捕获模型开篇吸引子）
//  3. Transition: 8×8 相邻值域转移分布（64 维，捕获序列反相关与跃迁动力学）
//  4. Position:   序列按索引 4 等分 × 16 个均匀值域桶（捕获前后段漂移）
//  5. LastDigit:  末位数字 v % 10 分布（10 维）
//
// 每个子空间仅维护逐响应归一化概率向量之和 SumVec 与平方范数之和 SumNormSq，
// 在 O(K) 空间内支持严格满足交换律与结合律的 Merge，并在 SeqCount ≥ 2 时计算精确无偏 U-统计量内积，
// 消除高维直方图跨样本量（如 repeats=1 巡检 vs repeats=2 全量）的 K/N 有限样本偏置。
type NumRandomProfile struct {
	MinVal       int
	MaxVal       int
	ValueSum     []float64
	ValueNormSq  float64
	PrefixSum    []float64
	PrefixNormSq float64
	PosSum       [numRandomChunks][numRandomRangeBins]float64
	PosNormSq    [numRandomChunks]float64
	DigitSum     [numRandomDigits]float64
	DigitNormSq  float64
	TransSum     [numRandomTransCells]float64
	TransNormSq  float64
	TotalInts    int
	SeqCount     int
}

// NumRandomDistance 五子空间响应级无偏余弦距离（U-Cos）分量及加权几何组合距离（值域 [0, 1]）。
type NumRandomDistance struct {
	Composite   float64
	ValueD      float64
	PrefixD     float64
	TransitionD float64
	PositionD   float64
	LastDigitD  float64
}

// NewNumRandomProfile 创建给定闭区间 [minVal, maxVal] 的特征累积器。
func NewNumRandomProfile(minVal, maxVal int) *NumRandomProfile {
	width := maxVal - minVal + 1
	if width < 1 {
		width = 1
	}
	return &NumRandomProfile{
		MinVal:    minVal,
		MaxVal:    maxVal,
		ValueSum:  make([]float64, width),
		PrefixSum: make([]float64, width),
	}
}

// AddSequence 向累积器追加一条有效整数序列（长度须 ≥ 4 且所有元素落在 [MinVal, MaxVal]）。
func (p *NumRandomProfile) AddSequence(seq []int) bool {
	if p == nil || len(seq) < numRandomChunks || p.MaxVal < p.MinVal {
		return false
	}
	width := p.MaxVal - p.MinVal + 1
	if len(p.ValueSum) != width || len(p.PrefixSum) != width {
		return false
	}
	for _, v := range seq {
		if v < p.MinVal || v > p.MaxVal {
			return false
		}
	}

	n := len(seq)
	invN := 1.0 / float64(n)
	valCounts := make([]int, width)
	prefWeights := make([]float64, width)
	var prefTotal float64
	var digCounts [numRandomDigits]int
	var posCounts [numRandomChunks][numRandomRangeBins]int
	var posLens [numRandomChunks]int

	q, r := n/numRandomChunks, n%numRandomChunks
	offset := 0
	for s := 0; s < numRandomChunks; s++ {
		chunkLen := q
		if s < r {
			chunkLen++
		}
		posLens[s] = chunkLen
		end := offset + chunkLen
		for idx := offset; idx < end; idx++ {
			v := seq[idx]
			u := v - p.MinVal
			valCounts[u]++

			w := math.Exp(-numRandomPrefixDecay * float64(idx) * invN)
			prefWeights[u] += w
			prefTotal += w

			bin := u * numRandomRangeBins / width
			if bin >= numRandomRangeBins {
				bin = numRandomRangeBins - 1
			}
			posCounts[s][bin]++

			dig := v % numRandomDigits
			if dig < 0 {
				dig += numRandomDigits
			}
			digCounts[dig]++
		}
		offset = end
	}

	var valSq, prefSq float64
	invPref := 1.0 / prefTotal
	for i := 0; i < width; i++ {
		if c := valCounts[i]; c > 0 {
			pv := float64(c) * invN
			p.ValueSum[i] += pv
			valSq += pv * pv
		}
		if pw := prefWeights[i]; pw > 0 {
			pp := pw * invPref
			p.PrefixSum[i] += pp
			prefSq += pp * pp
		}
	}
	p.ValueNormSq += valSq
	p.PrefixNormSq += prefSq

	for s := 0; s < numRandomChunks; s++ {
		invChunk := 1.0 / float64(posLens[s])
		var chunkSq float64
		for b := 0; b < numRandomRangeBins; b++ {
			if c := posCounts[s][b]; c > 0 {
				pp := float64(c) * invChunk
				p.PosSum[s][b] += pp
				chunkSq += pp * pp
			}
		}
		p.PosNormSq[s] += chunkSq
	}

	var digSq float64
	for d := 0; d < numRandomDigits; d++ {
		if c := digCounts[d]; c > 0 {
			pd := float64(c) * invN
			p.DigitSum[d] += pd
			digSq += pd * pd
		}
	}
	p.DigitNormSq += digSq

	var transCounts [numRandomTransCells]int
	for i := 0; i < n-1; i++ {
		b0 := (seq[i] - p.MinVal) * numRandomTransBins / width
		if b0 >= numRandomTransBins {
			b0 = numRandomTransBins - 1
		}
		b1 := (seq[i+1] - p.MinVal) * numRandomTransBins / width
		if b1 >= numRandomTransBins {
			b1 = numRandomTransBins - 1
		}
		transCounts[b0*numRandomTransBins+b1]++
	}
	invPairs := 1.0 / float64(n-1)
	var transSq float64
	for k := 0; k < numRandomTransCells; k++ {
		if c := transCounts[k]; c > 0 {
			pt := float64(c) * invPairs
			p.TransSum[k] += pt
			transSq += pt * pt
		}
	}
	p.TransNormSq += transSq

	p.TotalInts += n
	p.SeqCount++
	return true
}

// Merge 将同值域的另一累积器统计量合并到当前累积器（满足交换律与结合律）。
func (p *NumRandomProfile) Merge(other *NumRandomProfile) bool {
	if p == nil || other == nil || p.MinVal != other.MinVal || p.MaxVal != other.MaxVal ||
		len(p.ValueSum) != len(other.ValueSum) || len(p.PrefixSum) != len(other.PrefixSum) {
		return false
	}
	for i := range p.ValueSum {
		p.ValueSum[i] += other.ValueSum[i]
		p.PrefixSum[i] += other.PrefixSum[i]
	}
	p.ValueNormSq += other.ValueNormSq
	p.PrefixNormSq += other.PrefixNormSq
	for s := 0; s < numRandomChunks; s++ {
		for b := 0; b < numRandomRangeBins; b++ {
			p.PosSum[s][b] += other.PosSum[s][b]
		}
		p.PosNormSq[s] += other.PosNormSq[s]
	}
	for d := 0; d < numRandomDigits; d++ {
		p.DigitSum[d] += other.DigitSum[d]
	}
	p.DigitNormSq += other.DigitNormSq
	for k := 0; k < numRandomTransCells; k++ {
		p.TransSum[k] += other.TransSum[k]
	}
	p.TransNormSq += other.TransNormSq
	p.TotalInts += other.TotalInts
	p.SeqCount += other.SeqCount
	return true
}

// unbiasedCosineDist 计算两个响应级累积向量间的无偏余弦距离 sqrt(1 - cos_u) ∈ [0, 1]。
// 当两侧 SeqCount ≥ 2 且无偏自内积均为正时使用 U-统计量扣除逐响应有限样本方差偏置；
// 单序列（SeqCount == 1）或同侧完全不交支撑时平滑退化为无相消的标准余弦距离。
func unbiasedCosineDist(sumA []float64, normSqA float64, nA int, sumB []float64, normSqB float64, nB int) float64 {
	if len(sumA) == 0 || len(sumA) != len(sumB) || nA <= 0 || nB <= 0 {
		return math.NaN()
	}
	if nA == nB && normSqA == normSqB && floatSlicesEqual(sumA, sumB) {
		return 0
	}

	var dotAA, dotBB, dotAB float64
	for i := range sumA {
		a, b := sumA[i], sumB[i]
		dotAA += a * a
		dotBB += b * b
		dotAB += a * b
	}

	if nA >= 2 && nB >= 2 {
		aa := (dotAA - normSqA) / float64(nA*(nA-1))
		bb := (dotBB - normSqB) / float64(nB*(nB-1))
		ab := dotAB / float64(nA*nB)
		if aa > 1e-12 && bb > 1e-12 {
			denom := math.Sqrt(aa * bb)
			if ab >= denom {
				return 0
			}
			diff := (denom - ab) / denom
			if diff <= 1e-15 {
				return 0
			}
			if diff > 1 {
				return 1
			}
			return math.Sqrt(diff)
		}
	}

	nrmA := math.Sqrt(dotAA)
	nrmB := math.Sqrt(dotBB)
	if nrmA <= 1e-12 || nrmB <= 1e-12 {
		return math.NaN()
	}
	var sumSq float64
	for i := range sumA {
		d := sumA[i]/nrmA - sumB[i]/nrmB
		sumSq += d * d
	}
	v := 0.5 * sumSq
	if v <= 1e-15 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return math.Sqrt(v)
}

func floatSlicesEqual(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// CompareNumRandomProfiles 计算两侧累积器的五子空间响应级无偏余弦组合距离（U-Cos，0.45 : 0.20 : 0.15 : 0.10 : 0.10）。
func CompareNumRandomProfiles(a, b *NumRandomProfile) (NumRandomDistance, bool) {
	if a == nil || b == nil || a.MinVal != b.MinVal || a.MaxVal != b.MaxVal ||
		a.SeqCount <= 0 || b.SeqCount <= 0 || a.TotalInts <= 0 || b.TotalInts <= 0 {
		return NumRandomDistance{}, false
	}
	valD := unbiasedCosineDist(a.ValueSum, a.ValueNormSq, a.SeqCount, b.ValueSum, b.ValueNormSq, b.SeqCount)
	if math.IsNaN(valD) {
		return NumRandomDistance{}, false
	}
	prefD := unbiasedCosineDist(a.PrefixSum, a.PrefixNormSq, a.SeqCount, b.PrefixSum, b.PrefixNormSq, b.SeqCount)
	if math.IsNaN(prefD) {
		return NumRandomDistance{}, false
	}
	transD := unbiasedCosineDist(a.TransSum[:], a.TransNormSq, a.SeqCount, b.TransSum[:], b.TransNormSq, b.SeqCount)
	if math.IsNaN(transD) {
		return NumRandomDistance{}, false
	}
	var posSqSum float64
	for s := 0; s < numRandomChunks; s++ {
		cd := unbiasedCosineDist(a.PosSum[s][:], a.PosNormSq[s], a.SeqCount, b.PosSum[s][:], b.PosNormSq[s], b.SeqCount)
		if math.IsNaN(cd) {
			return NumRandomDistance{}, false
		}
		posSqSum += cd * cd
	}
	posD := math.Sqrt(posSqSum / float64(numRandomChunks))
	digD := unbiasedCosineDist(a.DigitSum[:], a.DigitNormSq, a.SeqCount, b.DigitSum[:], b.DigitNormSq, b.SeqCount)
	if math.IsNaN(digD) {
		return NumRandomDistance{}, false
	}

	comp := math.Sqrt(
		numRandomWeightValue*valD*valD +
			numRandomWeightPrefix*prefD*prefD +
			numRandomWeightTransition*transD*transD +
			numRandomWeightPosition*posD*posD +
			numRandomWeightLastDigit*digD*digD,
	)
	if comp > 1 {
		comp = 1
	}
	return NumRandomDistance{
		Composite:   comp,
		ValueD:      valD,
		PrefixD:     prefD,
		TransitionD: transD,
		PositionD:   posD,
		LastDigitD:  digD,
	}, true
}
