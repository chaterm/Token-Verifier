package stats

import (
	"math"
	"sort"
)

// mwu.go：Mann-Whitney U 检验与 Fisher p 值合并。
// think-effort 探针对两侧思考量分布做逐 cell 检验，
// 再把各 cell 的 p 值合并为探针级统计量。

// MannWhitneyU 双侧 Mann-Whitney U 检验（Wilcoxon 秩和的等价形式）。
// 非参数、对偏态分布稳健，适合思考量这类长尾数据。
// 实现用正态近似 + 连续性校正 + 并列值方差修正（mid-rank）：
// 探针场景样本量通常为 5~50，近似误差远小于分布形状假设带来的偏差；
// 任一侧为空返回 NaN，调用方应先剔除空 cell。
func MannWhitneyU(a, b []float64) float64 {
	na, nb := len(a), len(b)
	if na == 0 || nb == 0 {
		return math.NaN()
	}

	// 合并样本求秩；并列值取平均秩
	type valIdx struct {
		v float64
		i int
	}
	all := make([]valIdx, 0, na+nb)
	for _, x := range a {
		all = append(all, valIdx{x, 0})
	}
	for _, x := range b {
		all = append(all, valIdx{x, 1})
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].v < all[j].v })

	ranks := make([]float64, len(all))
	// tieGroups[i] = 该并列组的元素个数（用于方差修正）
	var tieGroups []int
	i := 0
	for i < len(all) {
		j := i
		for j+1 < len(all) && all[j+1].v == all[i].v {
			j++
		}
		avgRank := (float64(i)+float64(j))/2 + 1 // 秩从 1 开始
		for k := i; k <= j; k++ {
			ranks[k] = avgRank
		}
		tieGroups = append(tieGroups, j-i+1)
		i = j + 1
	}

	var rankSumA float64
	for k, s := range all {
		if s.i == 0 {
			rankSumA += ranks[k]
		}
	}

	uA := rankSumA - float64(na)*(float64(na)+1)/2
	meanU := float64(na) * float64(nb) / 2
	n := float64(na + nb)
	// 方差：并列值修正项 Σ(t³-t)/(N(N-1))
	var tieCorr float64
	for _, t := range tieGroups {
		if t > 1 {
			tf := float64(t)
			tieCorr += tf*tf*tf - tf
		}
	}
	variance := float64(na) * float64(nb) / 12 * ((n + 1) - tieCorr/(n*(n-1)))
	if variance <= 0 {
		// 全部样本同值：两组无法区分
		return 1
	}

	// 连续性校正：|U - mean| 减 0.5 再归一
	z := (math.Abs(uA-meanU) - 0.5) / math.Sqrt(variance)
	if z <= 0 {
		return 1
	}
	// 双侧 p = 2 * Φ(-|z|)
	return 2 * normSF(math.Abs(z))
}

// normSF 标准正态生存函数 Φ(-x) = 0.5·erfc(x/√2)，x ≥ 0。
func normSF(x float64) float64 {
	return 0.5 * math.Erfc(x/math.Sqrt2)
}

// FisherCombine Fisher 合并 p 值法：统计量 X² = -2Σln(pᵢ)，
// 在各 pᵢ 独立时服从自由度 2k 的卡方分布。返回合并后的双侧 p 值。
// 空输入或含 NaN/非正 p 值返回 NaN（调用方需先保证输入合法）。
func FisherCombine(pvals []float64) float64 {
	if len(pvals) == 0 {
		return math.NaN()
	}
	var sum float64
	for _, p := range pvals {
		if math.IsNaN(p) || p <= 0 {
			// p=0 会让 ln 爆炸：用最小正浮点顶替，等价于极大证据
			if p == 0 {
				p = math.SmallestNonzeroFloat64
			} else {
				return math.NaN()
			}
		}
		sum += math.Log(p)
	}
	x := -2 * sum
	return chi2SF(x, 2*len(pvals))
}
