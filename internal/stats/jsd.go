// Package stats 提供统计原语：JSD、卡方检验、分位数。
package stats

import (
	"math"
	"sort"
)

// Dist 一个离散分布：取值 → 出现次数。
type Dist map[string]int

// Total 样本总数。
func (d Dist) Total() int {
	n := 0
	for _, c := range d {
		n += c
	}
	return n
}

// FromSamples 从样本切片构建分布。
func FromSamples(samples []string) Dist {
	d := Dist{}
	for _, s := range samples {
		d[s]++
	}
	return d
}

// JSD 计算两个分布的 Jensen–Shannon 散度（base 2，值域 [0,1]）。
// 任一侧无样本时返回 1（视为最大分歧）；调用方应先剔除空样本 cell。
func JSD(p, q Dist) float64 {
	pn, qn := p.Total(), q.Total()
	if pn == 0 || qn == 0 {
		return 1
	}
	keys := make(map[string]bool, len(p)+len(q))
	for k := range p {
		keys[k] = true
	}
	for k := range q {
		keys[k] = true
	}
	var jsd float64
	for k := range keys {
		pi := float64(p[k]) / float64(pn)
		qi := float64(q[k]) / float64(qn)
		m := 0.5 * (pi + qi)
		if pi > 0 {
			jsd += 0.5 * pi * math.Log2(pi/m)
		}
		if qi > 0 {
			jsd += 0.5 * qi * math.Log2(qi/m)
		}
	}
	return jsd
}

// Mean 均值；空切片返回 NaN。
func Mean(xs []float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

// Quantile 分位数（线性插值）；空切片返回 NaN。
func Quantile(xs []float64, q float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	sorted := append([]float64(nil), xs...)
	sort.Float64s(sorted)
	if q <= 0 {
		return sorted[0]
	}
	if q >= 1 {
		return sorted[len(sorted)-1]
	}
	pos := q * float64(len(sorted)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return sorted[lo]
	}
	frac := pos - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}
