package probe

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/chaterm/token-verifier/internal/stats"
)

// tokenizerProbe 输入侧指纹探针：只看服务端上报的输入 token 数，
// 逐 cell 判两侧是否相等，汇总「匹配数 2×2 表」做卡方 / Fisher 检验。
type tokenizerProbe struct{}

// chi2MinExpected 期望频数低于此值时 2×2 表回退 Fisher 精确检验。
const chi2MinExpected = 5.0

// lowPowerCells 配对 cell 数低于此值时检验功效不足：
// 全不等时 n=4 的 p≈0.029 仍高于 α=0.01，n≥5 才能稳定判 fail。
const lowPowerCells = 5

func (tokenizerProbe) Meta() ProbeMeta {
	return ProbeMeta{
		ID: "tokenizer",
		// 必须按协议分层：不同协议的消息封装本身就导致输入 token 数不同，
		// pooling 会把协议差异混进信号（ARCHITECTURE.md §5）。
		CellKey:           []string{"question_id", "context_bucket", "protocol"},
		MinN:              1,
		ObservationSchema: "tokenizer/v1",
		StatKind:          StatPValue,
		Version:           "1",
	}
}

// tokenizerObs 观测结构：{"prompt_tokens": 128}，服务端上报值。
type tokenizerObs struct {
	PromptTokens int `json:"prompt_tokens"`
}

// Compare 的 minN 参数对 tokenizer 不生效：单侧 1 个样本即可判相等，
// 观测缺失/解析失败才记 insufficient（与样本量下限无关）。
func (p tokenizerProbe) Compare(pairs []CellPair, threshold float64, minN int) Verdict {
	v := Verdict{
		ProbeID:   p.Meta().ID,
		Threshold: threshold,
	}

	matched, total := 0, 0
	for _, pair := range pairs {
		a, okA := parseTokenizerValues(pair.A)
		b, okB := parseTokenizerValues(pair.B)

		detail := CellDetail{Key: pair.Key, NA: len(pair.A), NB: len(pair.B)}
		if !okA || !okB || len(a) == 0 || len(b) == 0 {
			// 观测缺失或解析失败：该 cell 不计入列联表
			detail.Insufficient = true
			detail.Extra = map[string]any{"reason": "observation_unavailable"}
			v.Cells = append(v.Cells, detail)
			continue
		}

		// 多重集相等（排序后逐元素比对）；repeats=1 时即单值相等
		match := multisetEqual(a, b)
		total++
		if match {
			matched++
		}
		one := 1.0
		zero := 0.0
		if match {
			detail.Stat = &one
		} else {
			detail.Stat = &zero
		}
		detail.Extra = map[string]any{"match": match, "a_values": a, "b_values": b}
		v.Cells = append(v.Cells, detail)
	}

	if total == 0 {
		v.Verdict = "inconclusive"
		if len(pairs) == 0 {
			v.Note = "两侧没有共同 cell"
		} else {
			v.Note = "所有配对 cell 的观测均不可用"
		}
		return v
	}

	// 匹配数 2×2 表：待测侧 [匹配, 不匹配] vs 基准侧 [全匹配, 0]。
	// 与参考实现 verifyL2 一致。功效边界要清楚：
	//   - 全不等时 n≥5 才能在 α=0.01 下判 fail（n=1 时 p=1，n=4 时 p≈0.029）；
	//   - 零星 1~2 处不等则 p 接近 1，不误报；
	//   - 失配数 <10 时必走 Fisher（期望频数不足），它比卡方保守 ——
	//     例如 10 cell 中 5 个不等：Fisher p≈0.033 pass，卡方 p≈0.0098 fail。
	//     一半失配本该报警的区域偏向放行，标定阈值与解读报告时需知道这一点。
	obsA := []int{matched, total - matched}
	obsB := []int{total, 0}
	pv, method := stats.Chi2Homogeneity(obsA, obsB, chi2MinExpected)

	v.Statistic = &pv
	r := ratio(StatPValue, pv, threshold)
	v.Ratio = &r
	v.Note = fmt.Sprintf("method=%s; matched %d/%d cells", method, matched, total)
	if total < lowPowerCells {
		v.Warning = fmt.Sprintf("配对 cell 数 %d < %d，检验功效不足：即使全部不等也可能判不出 fail"+
			"（n=1 时 p 恒为 1），建议增加题库题数", total, lowPowerCells)
	}
	if pv < threshold {
		v.Verdict = "fail"
	} else {
		v.Verdict = "pass"
	}
	return v
}

// parseTokenizerValues 提取 prompt_tokens 列表；任一条解析失败即返回 false，
// 该 cell 整体不计入 —— 半份数据算出的相等性没有意义。
func parseTokenizerValues(obs []Observation) ([]int, bool) {
	if len(obs) == 0 {
		return nil, false
	}
	values := make([]int, 0, len(obs))
	for _, raw := range obs {
		var o tokenizerObs
		if err := json.Unmarshal(raw, &o); err != nil {
			return nil, false
		}
		values = append(values, o.PromptTokens)
	}
	sort.Ints(values)
	return values, true
}

// multisetEqual 判断两个已排序的整数切片是否相等。
func multisetEqual(a, b []int) bool {
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
