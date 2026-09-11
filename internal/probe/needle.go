package probe

import (
	"encoding/json"
	"fmt"

	"github.com/chaterm/token-verifier/internal/stats"
)

// needleProbe 长上下文完整性探针：观测多埋点标记的逐位命中情况，
// 按「一个埋点 = 一个统计单元」聚合两侧命中数做 2×2 卡方 / Fisher。
// 两侧都不假定全中 —— 比的是召回率的差异（参考实现的语义），
// 基准侧自己漏掉的埋点不会制造假警报。
type needleProbe struct{}

func (needleProbe) Meta() ProbeMeta {
	return ProbeMeta{
		ID: "needle",
		// 标记召回是模型行为，跨协议可比；分层只会让每个 cell 样本更薄
		CellKey:           []string{"question_id", "context_bucket"},
		MinN:              1,
		ObservationSchema: "needle/v1",
		StatKind:          StatPValue,
		Version:           "1",
	}
}

// needleObs 观测结构：{"matched": [true, false, ...]}，
// 布尔数组按埋点位置序号对齐（一个埋点一个布尔）。
type needleObs struct {
	Matched []bool `json:"matched"`
}

// needleUnit 一个统计单元：(cell, 埋点序号) 的两侧命中计数。
type needleUnit struct {
	cellIdx  int
	needle   int
	aHit, aN int
	bHit, bN int
}

func (p needleProbe) Compare(pairs []CellPair, threshold float64, minN int) Verdict {
	if minN < 1 {
		minN = p.Meta().MinN
	}
	v := Verdict{
		ProbeID:   p.Meta().ID,
		Threshold: threshold,
	}

	units := map[string]*needleUnit{} // "cellIdx|needleIdx" → 计数
	parseFailed := 0
	for ci, pair := range pairs {
		aCounts := tallyNeedle(pair.A, &parseFailed)
		bCounts := tallyNeedle(pair.B, &parseFailed)
		// 埋点数取两侧出现过的最大序号，缺位按未命中计（长度不齐视为漏）
		n := 0
		for _, c := range aCounts {
			if len(c) > n {
				n = len(c)
			}
		}
		for _, c := range bCounts {
			if len(c) > n {
				n = len(c)
			}
		}
		detail := CellDetail{Key: pair.Key, NA: len(pair.A), NB: len(pair.B)}
		aHits, bHits := 0, 0
		aTotal, bTotal := 0, 0
		for i := 0; i < n; i++ {
			key := fmt.Sprintf("%d|%d", ci, i)
			u := units[key]
			if u == nil {
				u = &needleUnit{cellIdx: ci, needle: i}
				units[key] = u
			}
			for _, m := range aCounts {
				u.aN++
				aTotal++
				if i < len(m) && m[i] {
					u.aHit++
					aHits++
				}
			}
			for _, m := range bCounts {
				u.bN++
				bTotal++
				if i < len(m) && m[i] {
					u.bHit++
					bHits++
				}
			}
		}
		if aTotal > 0 || bTotal > 0 {
			rateA, rateB := 0.0, 0.0
			if aTotal > 0 {
				rateA = float64(aHits) / float64(aTotal)
			}
			if bTotal > 0 {
				rateB = float64(bHits) / float64(bTotal)
			}
			detail.Extra = map[string]any{
				"needles": n,
				"a_hit":   aHits, "a_total": aTotal, "a_rate": rateA,
				"b_hit": bHits, "b_total": bTotal, "b_rate": rateB,
			}
		}
		v.Cells = append(v.Cells, detail)
	}

	// 汇总所有单元的 2×2 表：[命中, 未命中] 两侧各一行
	obsA := []int{0, 0}
	obsB := []int{0, 0}
	usedUnits, skippedUnits := 0, 0
	for _, u := range units {
		// 单元级样本下限：任一侧不足则该单元剔除（二值观测噪声大）
		if u.aN < minN || u.bN < minN {
			skippedUnits++
			continue
		}
		obsA[0] += u.aHit
		obsA[1] += u.aN - u.aHit
		obsB[0] += u.bHit
		obsB[1] += u.bN - u.bHit
		usedUnits++
	}

	if usedUnits == 0 {
		v.Verdict = "inconclusive"
		switch {
		case len(pairs) == 0:
			v.Note = "两侧没有共同 cell"
		case skippedUnits > 0:
			v.Note = fmt.Sprintf("所有埋点单元的有效样本量均不足（n < %d）", minN)
		default:
			v.Note = "所有配对 cell 的观测均不可用"
		}
		return v
	}

	pv, method := stats.Chi2Homogeneity(obsA, obsB, chi2MinExpected)
	v.Statistic = &pv
	r := ratio(StatPValue, pv, threshold)
	v.Ratio = &r
	totalA, totalB := obsA[0]+obsA[1], obsB[0]+obsB[1]
	v.Note = fmt.Sprintf("method=%s; 埋点单元 %d 个（剔除 %d）; A 命中 %d/%d, B 命中 %d/%d; 观测解析失败 %d 条",
		method, usedUnits, skippedUnits, obsA[0], totalA, obsB[0], totalB, parseFailed)
	if usedUnits < lowPowerCells {
		v.Warning = fmt.Sprintf("埋点单元数 %d < %d，检验功效不足：召回率差异需要更多埋点或重复才能稳定判出"+
			"，建议增加 needle_positions 或 repeats", usedUnits, lowPowerCells)
	}
	if pv < threshold {
		v.Verdict = "fail"
	} else {
		v.Verdict = "pass"
	}
	return v
}

// tallyNeedle 把一侧观测解析成逐条的布尔数组；解析失败计数后跳过该条。
func tallyNeedle(obs []Observation, failed *int) [][]bool {
	out := make([][]bool, 0, len(obs))
	for _, raw := range obs {
		var o needleObs
		if err := json.Unmarshal(raw, &o); err != nil || o.Matched == nil {
			*failed++
			continue
		}
		out = append(out, o.Matched)
	}
	return out
}
