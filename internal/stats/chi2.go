package stats

import "math"

// Chi2Homogeneity 检验两组观测频数是否来自同一分布（2×k 列联表）。
// 返回 p 值与所用方法（chi2/fisher）。
//
// 期望频数全部 >= minExpected 时用卡方近似；否则 2×2 表回退
// Fisher 精确检验（小样本下卡方近似在尾部不可靠）。
func Chi2Homogeneity(obsA, obsB []int, minExpected float64) (p float64, method string) {
	if len(obsA) != len(obsB) || len(obsA) == 0 {
		return math.NaN(), "invalid"
	}
	k := len(obsA)
	nA, nB := sumInts(obsA), sumInts(obsB)
	n := nA + nB
	if n == 0 {
		return math.NaN(), "empty"
	}

	// 检查期望频数是否都够大
	small := false
	for i := 0; i < k; i++ {
		col := float64(obsA[i] + obsB[i])
		eA := col * float64(nA) / float64(n)
		eB := col * float64(nB) / float64(n)
		if eA < minExpected || eB < minExpected {
			small = true
			break
		}
	}
	if small {
		if k == 2 {
			return fisherExact2x2(obsA[0], obsA[1], obsB[0], obsB[1]), "fisher"
		}
		// k>2 的小样本不做蒙特卡洛，仍用卡方近似，由调用方知悉 method
	}

	chi2 := 0.0
	for i := 0; i < k; i++ {
		col := float64(obsA[i] + obsB[i])
		eA := col * float64(nA) / float64(n)
		eB := col * float64(nB) / float64(n)
		if eA > 0 {
			chi2 += math.Pow(float64(obsA[i])-eA, 2) / eA
		}
		if eB > 0 {
			chi2 += math.Pow(float64(obsB[i])-eB, 2) / eB
		}
	}
	return chi2SF(chi2, k-1), "chi2"
}

func sumInts(xs []int) int {
	s := 0
	for _, x := range xs {
		s += x
	}
	return s
}

// chi2SF 卡方分布的生存函数 P(X > x)，df 自由度。
// 用正则化上不完全 Gamma 精确计算，不用 Wilson-Hilferty 近似 ——
// 判定发生在 α=0.01 量级的尾部，近似误差会直接翻转结论。
func chi2SF(x float64, df int) float64 {
	if x <= 0 {
		return 1
	}
	if df <= 0 {
		return math.NaN()
	}
	return gammaQ(float64(df)/2, x/2)
}

// gammaQ 正则化上不完全 Gamma 函数 Q(a,x) = 1 - P(a,x)。
// x < a+1 用级数展开，否则用连分式（Numerical Recipes 标准做法）。
func gammaQ(a, x float64) float64 {
	if x < a+1 {
		return 1 - gammaPSeries(a, x)
	}
	return gammaQCF(a, x)
}

// gammaPSeries 级数展开求 P(a,x)。
func gammaPSeries(a, x float64) float64 {
	const maxIter = 500
	const eps = 3e-16
	ap := a
	sum := 1.0 / a
	del := sum
	for i := 0; i < maxIter; i++ {
		ap++
		del *= x / ap
		sum += del
		if math.Abs(del) < math.Abs(sum)*eps {
			break
		}
	}
	return sum * math.Exp(-x+a*math.Log(x)-logGamma(a))
}

// gammaQCF 连分式（Lentz 算法）求 Q(a,x)。
func gammaQCF(a, x float64) float64 {
	const maxIter = 500
	const eps = 3e-16
	const tiny = 1e-300
	b := x + 1 - a
	c := 1 / tiny
	d := 1 / b
	h := d
	for i := 1; i <= maxIter; i++ {
		an := -float64(i) * (float64(i) - a)
		b += 2
		d = an*d + b
		if math.Abs(d) < tiny {
			d = tiny
		}
		c = b + an/c
		if math.Abs(c) < tiny {
			c = tiny
		}
		d = 1 / d
		del := d * c
		h *= del
		if math.Abs(del-1) < eps {
			break
		}
	}
	return h * math.Exp(-x+a*math.Log(x)-logGamma(a))
}

// logGamma 对数 Gamma 函数，a>0。
func logGamma(a float64) float64 {
	r, _ := math.Lgamma(a)
	return r
}

// fisherExact2x2 2×2 表的 Fisher 精确检验，返回双侧 p 值。
// 表为 [[a b] [c d]]，行和列和固定，对超几何分布求
// 「概率不大于观测表」的所有表之和。
func fisherExact2x2(a, b, c, d int) float64 {
	n := a + b + c + d
	if n == 0 {
		return math.NaN()
	}
	rowA, rowB := a+b, c+d
	colA := a + c

	// 超几何概率 P(X=x) = C(rowA,x)·C(rowB,colA-x) / C(n,colA)
	// 用对数组合数避免中间值溢出。
	logDen := logComb(n, colA)
	hypergeom := func(x int) float64 {
		if x < 0 || x > rowA || colA-x < 0 || colA-x > rowB {
			return 0
		}
		return math.Exp(logComb(rowA, x) + logComb(rowB, colA-x) - logDen)
	}

	obsProb := hypergeom(a)
	minA := max(0, colA-rowB)
	maxA := min(rowA, colA)
	p := 0.0
	for x := minA; x <= maxA; x++ {
		prob := hypergeom(x)
		// 容差吸收浮点噪声：概率相等（对称表）时必须都计入
		if prob <= obsProb*(1+1e-7) {
			p += prob
		}
	}
	return math.Min(p, 1)
}

// logComb 对数组合数 ln C(n,k)，用 lgamma 保证大数不溢出。
func logComb(n, k int) float64 {
	if k < 0 || k > n {
		return math.Inf(-1)
	}
	lg := func(x float64) float64 { r, _ := math.Lgamma(x); return r }
	return lg(float64(n+1)) - lg(float64(k+1)) - lg(float64(n-k+1))
}
