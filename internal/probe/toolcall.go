package probe

import (
	"encoding/json"
	"fmt"
	"math"

	"github.com/chaterm/token-verifier/internal/stats"
)

// toolcallProbe agent 行为探针：观测首个工具的选择分布与参数语法合法率。
// 两个证据分量（逐 cell 计算，聚合后取更差者定判）：
//   - 工具选择分布：逐 cell 两侧 JSD（distance 语义），探针级取达标 cell 均值
//     —— 与 onetoken 同构，阈值是距离上限；
//   - args_valid 合法率：逐 cell 累计 [valid, invalid] 计数，池化成探针级
//     2×2 表做卡方 / Fisher（pvalue 语义），阈值同时充当 alpha。
//
// 一个阈值两用是刻意的：使用者给 toolcall 配一个数（如 0.05），
// JSD 分量按「均值 > 0.05 即超」判，合法率分量按「p < 0.05 即超」判，
// 任一分量超阈 → fail。parallel 率只进报告明细，不参与判定
// （repeats 内方差大，噪声误判风险高于信号价值）。
//
// Verdict.Statistic 只承载 JSD 分量，Ratio 取两分量的更差者 ——
// 合法率分量主导时二者会「对不上」，Note 里印了两个分量各自的值。
type toolcallProbe struct{}

func (toolcallProbe) Meta() ProbeMeta {
	return ProbeMeta{
		ID: "toolcall",
		// 工具选择是模型行为，跨协议可比（工具字段形态由适配器归一）
		CellKey:           []string{"question_id", "context_bucket"},
		MinN:              5,
		ObservationSchema: "toolcall/v1",
		StatKind:          StatDistance,
		Version:           "1",
	}
}

// toolcallObs 观测结构：{"tool": "get_weather", "args_valid": true,
// "parallel": false, "num_calls": 1}。tool 为首个调用的工具名。
type toolcallObs struct {
	Tool      string `json:"tool"`
	ArgsValid bool   `json:"args_valid"`
	Parallel  bool   `json:"parallel"`
	NumCalls  int    `json:"num_calls"`
}

func (p toolcallProbe) Compare(pairs []CellPair, threshold float64, minN int) Verdict {
	if minN < 1 {
		minN = p.Meta().MinN
	}
	v := Verdict{
		ProbeID:   p.Meta().ID,
		Threshold: threshold,
	}

	var jsds []float64
	// 合法率证据池：跨 cell 累计两侧 [valid, invalid]
	poolA := []int{0, 0}
	poolB := []int{0, 0}
	pooledCells := 0
	totalParseFailed := 0

	for _, pair := range pairs {
		a, badA := parseToolcallObs(pair.A)
		b, badB := parseToolcallObs(pair.B)
		totalParseFailed += badA + badB
		detail := CellDetail{Key: pair.Key, NA: len(a), NB: len(b)}
		if badA+badB > 0 {
			detail.Extra = map[string]any{"parse_failed": badA + badB}
		}

		// 样本量不足的 cell 剔除而非降权（分布/比率都对样本量敏感）
		if len(a) < minN || len(b) < minN {
			detail.Insufficient = true
			v.Cells = append(v.Cells, detail)
			continue
		}

		// 分量一：工具选择分布 JSD
		distA, distB := toolNameDist(a), toolNameDist(b)
		jsd := stats.JSD(distA, distB)
		jsds = append(jsds, jsd)
		detail.Stat = &jsd

		// 分量二：合法率计数（进探针级池）
		va, ia := validCounts(a)
		vb, ib := validCounts(b)
		poolA[0] += va
		poolA[1] += ia
		poolB[0] += vb
		poolB[1] += ib
		pooledCells++

		detail.Extra = mergeExtra(detail.Extra, map[string]any{
			"jsd":          jsd,
			"a_dist":       map[string]int(distA),
			"b_dist":       map[string]int(distB),
			"a_args_valid": va, "a_args_invalid": ia,
			"b_args_valid": vb, "b_args_invalid": ib,
			"a_parallel": parallelCount(a), "b_parallel": parallelCount(b),
		})
		v.Cells = append(v.Cells, detail)
	}

	if len(jsds) == 0 {
		v.Verdict = "inconclusive"
		if len(pairs) == 0 {
			v.Note = "两侧没有共同 cell"
		} else {
			v.Note = fmt.Sprintf("所有配对 cell 的有效样本量均不足（n < %d）", minN)
		}
		return v
	}

	jsdMean := stats.Mean(jsds)
	v.Statistic = &jsdMean
	ratioJSD := ratio(StatDistance, jsdMean, threshold)

	// 合法率分量：池化 2×2 → 卡方/Fisher。两侧计数完全一致（含全 valid）时
	// p=1，不贡献 fail。任一侧无计数（理论上不会，jsds 非空即有样本）跳过。
	ratioValid := 0.0
	pValid, method := 1.0, "n/a"
	if poolA[0]+poolA[1] > 0 && poolB[0]+poolB[1] > 0 {
		pValid, method = stats.Chi2Homogeneity(poolA, poolB, chi2MinExpected)
		if math.IsNaN(pValid) {
			pValid = 1
		}
		ratioValid = ratio(StatPValue, pValid, threshold)
	}

	r := math.Max(ratioJSD, ratioValid)
	if math.IsInf(r, 1) {
		r = math.MaxFloat64
	}
	v.Ratio = &r
	v.Note = fmt.Sprintf("JSD mean=%.4f (ratio %.2f); args_valid %s p=%.4g (ratio %.2f); %d cells; 观测解析失败 %d 条",
		jsdMean, ratioJSD, method, pValid, ratioValid, len(jsds), totalParseFailed)
	if r > 1 {
		v.Verdict = "fail"
		if ratioValid > ratioJSD {
			v.Note += "; 主因: 参数合法率差异"
		} else {
			v.Note += "; 主因: 工具选择分布差异"
		}
	} else {
		v.Verdict = "pass"
	}
	return v
}

// parseToolcallObs 提取可解析的观测；解析失败单独计数跳过。
func parseToolcallObs(obs []Observation) ([]toolcallObs, int) {
	out := make([]toolcallObs, 0, len(obs))
	bad := 0
	for _, raw := range obs {
		var o toolcallObs
		if err := json.Unmarshal(raw, &o); err != nil || o.Tool == "" {
			bad++
			continue
		}
		out = append(out, o)
	}
	return out, bad
}

// toolNameDist 首个工具名的离散分布。
func toolNameDist(obs []toolcallObs) stats.Dist {
	d := stats.Dist{}
	for _, o := range obs {
		d[o.Tool]++
	}
	return d
}

// validCounts [valid, invalid] 计数。
func validCounts(obs []toolcallObs) (int, int) {
	v, i := 0, 0
	for _, o := range obs {
		if o.ArgsValid {
			v++
		} else {
			i++
		}
	}
	return v, i
}

// parallelCount 并行调用（num_calls>1）的观测条数，仅报告展示。
func parallelCount(obs []toolcallObs) int {
	n := 0
	for _, o := range obs {
		if o.Parallel {
			n++
		}
	}
	return n
}
