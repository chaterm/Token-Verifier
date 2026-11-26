package probe

import (
	"encoding/json"
	"fmt"

	"github.com/chaterm/token-verifier/internal/stats"
)

// numrandomProbe 数字随机序列指纹探针（响应级无偏余弦距离 U-Cos 双样本检验）：
// 每次请求收集约 300 个 [min_value, max_value] 整数序列，提取五子空间：
//   - 全序列边际值分布（权重 0.45）
//   - 开篇指数衰减前缀分布 w_i = exp(-5*i/L)（权重 0.20）
//   - 8×8 相邻值域转移分布（权重 0.15）
//   - 4 段 × 16 值域位置分块分布（权重 0.10）
//   - 末位数字 v % 10 分布（权重 0.10）
//
// 单格明细记录各自组合距离供定位异常题目；
// 探针级统计量对所有达标配对 cell 跨格池化后计算响应级无偏余弦距离（U-Cos），
// 扣除逐响应有限样本方差偏置，消除 repeats=1 巡检与 repeats=2 全量混比时的 K/N 偏置碰撞。
type numrandomProbe struct{}

const numrandomMinN = 1

func (numrandomProbe) Meta() ProbeMeta {
	return ProbeMeta{
		ID:                "numrandom",
		Version:           "1",
		CellKey:           []string{"question_id", "context_bucket"},
		MinN:              numrandomMinN,
		ObservationSchema: "numrandom/v1",
		StatKind:          StatDistance,
	}
}

type numrandomObs struct {
	Values      []int `json:"values"`
	MinValue    int   `json:"min_value"`
	MaxValue    int   `json:"max_value"`
	TargetCount int   `json:"target_count"`
}

func (p numrandomProbe) Compare(pairs []CellPair, threshold float64, minN int) Verdict {
	if minN < 1 {
		minN = p.Meta().MinN
	}
	v := Verdict{
		ProbeID:   p.Meta().ID,
		Threshold: threshold,
	}
	if len(pairs) == 0 {
		v.Verdict = "inconclusive"
		v.Note = "两侧没有共同 cell"
		return v
	}

	var globalA, globalB *stats.NumRandomProfile
	qualifying := 0
	paramMismatch := false

	for _, pair := range pairs {
		obsA, badA := parseNumrandomObs(pair.A)
		obsB, badB := parseNumrandomObs(pair.B)

		detail := CellDetail{
			Key: pair.Key,
			NA:  len(obsA),
			NB:  len(obsB),
		}
		if badA+badB > 0 {
			detail.Extra = map[string]any{"parse_failed": badA + badB}
		}

		if len(obsA) < minN || len(obsB) < minN {
			detail.Insufficient = true
			detail.Extra = mergeExtra(detail.Extra, map[string]any{"reason": "insufficient_sequences"})
			v.Cells = append(v.Cells, detail)
			continue
		}

		minVal, maxVal, okRange := commonNumrandomRange(obsA, obsB)
		if !okRange || (globalA != nil && (globalA.MinVal != minVal || globalA.MaxVal != maxVal)) {
			paramMismatch = true
			detail.Insufficient = true
			detail.Extra = mergeExtra(detail.Extra, map[string]any{"reason": "params_mismatch"})
			v.Cells = append(v.Cells, detail)
			continue
		}

		profA, targetSumA, okA := buildCellProfile(obsA, minVal, maxVal)
		profB, targetSumB, okB := buildCellProfile(obsB, minVal, maxVal)
		if !okA || !okB {
			detail.Insufficient = true
			detail.Extra = mergeExtra(detail.Extra, map[string]any{"reason": "invalid_sequence"})
			v.Cells = append(v.Cells, detail)
			continue
		}

		cellDist, okDist := stats.CompareNumRandomProfiles(profA, profB)
		if !okDist {
			detail.Insufficient = true
			detail.Extra = mergeExtra(detail.Extra, map[string]any{"reason": "empty_profile"})
			v.Cells = append(v.Cells, detail)
			continue
		}

		statVal := cellDist.Composite
		detail.Stat = &statVal
		var covA, covB float64
		if targetSumA > 0 {
			covA = float64(profA.TotalInts) / float64(targetSumA)
		}
		if targetSumB > 0 {
			covB = float64(profB.TotalInts) / float64(targetSumB)
		}
		detail.Extra = mergeExtra(detail.Extra, map[string]any{
			"value_dist":      cellDist.ValueD,
			"prefix_dist":     cellDist.PrefixD,
			"transition_dist": cellDist.TransitionD,
			"position_dist":   cellDist.PositionD,
			"last_digit_dist": cellDist.LastDigitD,
			"a_total_ints":    profA.TotalInts,
			"b_total_ints":    profB.TotalInts,
			"a_coverage":      covA,
			"b_coverage":      covB,
		})
		v.Cells = append(v.Cells, detail)

		if globalA == nil {
			globalA = stats.NewNumRandomProfile(minVal, maxVal)
			globalB = stats.NewNumRandomProfile(minVal, maxVal)
		}
		globalA.Merge(profA)
		globalB.Merge(profB)
		qualifying++
	}

	if paramMismatch {
		v.Verdict = "inconclusive"
		v.Note = "两侧或跨题目的 numrandom 值域参数不一致（params_mismatch）"
		return v
	}
	if qualifying == 0 {
		v.Verdict = "inconclusive"
		v.Note = fmt.Sprintf("所有配对 cell 的有效序列数均不足（n < %d）", minN)
		return v
	}
	// 覆盖率门控：至少 80% 的配对 cell 达标，防止大量截断/失败时仅凭个别存活题给出假性 pass
	if qualifying*5 < len(pairs)*4 {
		v.Verdict = "inconclusive"
		v.Note = fmt.Sprintf("有效配对 cell 覆盖率不足（%d/%d < 80%%，单侧至少 %d 条有效序列）", qualifying, len(pairs), minN)
		return v
	}

	pooledDist, ok := stats.CompareNumRandomProfiles(globalA, globalB)
	if !ok {
		v.Verdict = "inconclusive"
		v.Note = "池化整数分布为空，无法计算 U-Cos 距离"
		return v
	}

	stat := pooledDist.Composite
	v.Statistic = &stat
	r := ratio(StatDistance, stat, threshold)
	v.Ratio = &r
	v.Note = fmt.Sprintf("pooled U-Cos=%.4f (val=%.4f, pre=%.4f, tr=%.4f, pos=%.4f, dig=%.4f); %d/%d cells; ints A=%d B=%d",
		pooledDist.Composite, pooledDist.ValueD, pooledDist.PrefixD, pooledDist.TransitionD,
		pooledDist.PositionD, pooledDist.LastDigitD,
		qualifying, len(pairs), globalA.TotalInts, globalB.TotalInts)
	if stat > threshold {
		v.Verdict = "fail"
	} else {
		v.Verdict = "pass"
	}
	return v
}

func parseNumrandomObs(rawList []Observation) ([]numrandomObs, int) {
	out := make([]numrandomObs, 0, len(rawList))
	bad := 0
	for _, raw := range rawList {
		var o numrandomObs
		if err := json.Unmarshal(raw, &o); err != nil || len(o.Values) < 4 || o.MinValue < 1 || o.MaxValue <= o.MinValue {
			bad++
			continue
		}
		out = append(out, o)
	}
	return out, bad
}

func commonNumrandomRange(a, b []numrandomObs) (int, int, bool) {
	if len(a) == 0 || len(b) == 0 {
		return 0, 0, false
	}
	minV, maxV := a[0].MinValue, a[0].MaxValue
	for _, o := range a[1:] {
		if o.MinValue != minV || o.MaxValue != maxV {
			return 0, 0, false
		}
	}
	for _, o := range b {
		if o.MinValue != minV || o.MaxValue != maxV {
			return 0, 0, false
		}
	}
	return minV, maxV, true
}

func buildCellProfile(obs []numrandomObs, minVal, maxVal int) (*stats.NumRandomProfile, int, bool) {
	prof := stats.NewNumRandomProfile(minVal, maxVal)
	targetSum := 0
	for _, o := range obs {
		// 按 target_count 截断：采集端已截断的新观测是恒等操作；
		// 对采集端未截断的历史 rawdata 在此兜底，保证旧数据重算与新生成语义一致。
		values := o.Values
		if o.TargetCount > 0 && len(values) > o.TargetCount {
			values = values[:o.TargetCount]
		}
		if !prof.AddSequence(values) {
			return nil, 0, false
		}
		if o.TargetCount > 0 {
			targetSum += o.TargetCount
		}
	}
	return prof, targetSum, prof.SeqCount > 0
}
