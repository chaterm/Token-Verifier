package probe

import (
	"encoding/json"
	"fmt"
	"math"

	"github.com/chaterm/token-verifier/internal/stats"
)

// thinkEffortProbe 思考行为探针：观测思考量的分布，按强度级别与协议分组。
// 逐 cell 做双侧 Mann-Whitney U 检验（非参数，对思考量这类偏态分布稳健），
// 探针级用 Fisher 合并 p 值 —— 强度参数被削减的端点会在 high 档的 cell 上
// 暴露出显著差异，合并让任一档的强证据都能触发判定。
//
// cell_key 含 protocol（与 tokenizer 同构）：观测单位由协议静态决定
// （adapter.ReasoningUnit —— openai 系上报思考 token 数，anthropic 系
// 交付思考文本、以 rune 数计量），秩检验只要求 cell 内两侧单位一致，
// 按协议分层后天然满足；代价是样本按协议切碎，repeats 需按最稀的
// 协议份额配够 min_n。
type thinkEffortProbe struct{}

// thinkEffortHistBins 思考量直方图的 bin 数：终端渲染宽度（~20 行）内
// 足以看出分布分离，又不至于每 bin 只有零星样本。
const thinkEffortHistBins = 16

func (thinkEffortProbe) Meta() ProbeMeta {
	return ProbeMeta{
		ID: "think-effort",
		// 强度是符号级别（low/medium/high），不同级别的思考预算本就不同，
		// 必须分组比较，否则级别差异会淹没端点削减的信号。
		// protocol 分层保证 cell 内观测单位一致（tokens vs chars）。
		CellKey:           []string{"question_id", "context_bucket", "thinking_effort", "protocol"},
		MinN:              5,
		ObservationSchema: "think-effort/v2",
		StatKind:          StatPValue,
		Version:           "2",
	}
}

// thinkEffortObs 观测结构：{"reasoning_value": 512, "unit": "tokens"}。
// unit ∈ {tokens, chars}，由采集侧按协议写入（adapter.ReasoningUnit）。
// 跨单位混比在采集期就被 cell_key 的 protocol 分层排除，这里只需校验形状。
type thinkEffortObs struct {
	ReasoningValue int    `json:"reasoning_value"`
	Unit           string `json:"unit"`
}

func (p thinkEffortProbe) Compare(pairs []CellPair, threshold float64, minN int) Verdict {
	if minN < 1 {
		minN = p.Meta().MinN
	}
	v := Verdict{
		ProbeID:   p.Meta().ID,
		Threshold: threshold,
	}

	var pvals []float64
	for _, pair := range pairs {
		a, unitA, badA := parseReasoningValues(pair.A)
		b, unitB, badB := parseReasoningValues(pair.B)
		detail := CellDetail{Key: pair.Key, NA: len(a), NB: len(b)}
		if badA+badB > 0 {
			detail.Extra = map[string]any{"parse_failed": badA + badB}
		}
		if len(a) < minN || len(b) < minN {
			detail.Insufficient = true
			if detail.Extra == nil {
				detail.Extra = map[string]any{}
			}
			detail.Extra["reason"] = "insufficient_samples"
			v.Cells = append(v.Cells, detail)
			continue
		}
		if unitA != unitB {
			// 采集期按协议分层已排除跨单位混比；出现即数据异常，
			// 不静默检验（秩检验在混合单位下无意义）
			detail.Insufficient = true
			if detail.Extra == nil {
				detail.Extra = map[string]any{}
			}
			detail.Extra["reason"] = "unit_mismatch"
			v.Cells = append(v.Cells, detail)
			continue
		}
		pv := stats.MannWhitneyU(a, b)
		if math.IsNaN(pv) {
			detail.Insufficient = true
			if detail.Extra == nil {
				detail.Extra = map[string]any{}
			}
			detail.Extra["reason"] = "test_unavailable"
			v.Cells = append(v.Cells, detail)
			continue
		}
		pp := pv
		detail.Stat = &pp
		detail.Extra = mergeExtra(detail.Extra, map[string]any{
			"p_value":  pv,
			"unit":     unitA,
			"a_median": stats.Quantile(a, 0.5),
			"b_median": stats.Quantile(b, 0.5),
			"a_mean":   stats.Mean(a),
			"b_mean":   stats.Mean(b),
			// 思考量是连续量：联合直方图供 verbose 报告渲染分布形状
			"hist": stats.JointHist(a, b, thinkEffortHistBins),
		})
		v.Cells = append(v.Cells, detail)
		pvals = append(pvals, pv)
	}

	if len(pvals) == 0 {
		v.Verdict = "inconclusive"
		if len(pairs) == 0 {
			v.Note = "两侧没有共同 cell"
		} else {
			v.Note = fmt.Sprintf("所有配对 cell 的有效样本量均不足（n < %d）或观测不可用", minN)
		}
		return v
	}

	combined := stats.FisherCombine(pvals)
	if math.IsNaN(combined) {
		v.Verdict = "inconclusive"
		v.Note = "Fisher 合并失败（存在非法 p 值）"
		return v
	}
	v.Statistic = &combined
	r := ratio(StatPValue, combined, threshold)
	v.Ratio = &r
	v.Note = fmt.Sprintf("method=mann-whitney-u+fisher; %d 个 cell 参与合并", len(pvals))
	if combined < threshold {
		v.Verdict = "fail"
	} else {
		v.Verdict = "pass"
	}
	return v
}

// parseReasoningValues 提取 reasoning_value 列表与生效单位；解析失败或形状
// 非法（负值、未知单位、同侧单位不一致）单独计数跳过（与 onetoken 同语义：
// 坏观测不污染分布，但要在报告里可见）。
func parseReasoningValues(obs []Observation) ([]float64, string, int) {
	out := make([]float64, 0, len(obs))
	unit := ""
	bad := 0
	for _, raw := range obs {
		var o thinkEffortObs
		if err := json.Unmarshal(raw, &o); err != nil || o.ReasoningValue < 0 ||
			(o.Unit != "tokens" && o.Unit != "chars") || (unit != "" && o.Unit != unit) {
			bad++
			continue
		}
		unit = o.Unit
		out = append(out, float64(o.ReasoningValue))
	}
	return out, unit, bad
}

// mergeExtra 合并 Extra map（nil 安全）。
func mergeExtra(base, add map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(add))
	for k, x := range base {
		out[k] = x
	}
	for k, x := range add {
		out[k] = x
	}
	return out
}
