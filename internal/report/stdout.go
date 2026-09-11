// Package report 把比较结果渲染为 stdout / JSON / JUnit 三种形态。
package report

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"

	"github.com/chaterm/token-verifier/internal/compare"
	"github.com/chaterm/token-verifier/internal/probe"
)

// Stdout 渲染 README 风格的人读报告。
func Stdout(w io.Writer, res *compare.Result) {
	fmt.Fprintf(w, "plan_digest  %s   (match)\n", truncateDigest(res.PlanDigest))
	fmt.Fprintf(w, "coverage     %d/%d probes compared\n", res.Compared, res.Planned)
	if res.IncompleteA || res.IncompleteB {
		fmt.Fprintf(w, "warning      至少一侧 rawData 缺 aggregates，数据不完整\n")
	}
	fmt.Fprintln(w)

	// 探针表
	fmt.Fprintf(w, "%-18s %-11s %-10s %-8s %s\n", "PROBE", "STATISTIC", "THRESHOLD", "SOURCE", "VERDICT")
	for _, v := range res.Verdicts {
		fmt.Fprintf(w, "%-18s %-11s %-10s %-8s %s\n",
			v.ProbeID, statisticText(v), thresholdText(v), v.ThresholdSource, strings.ToUpper(v.Verdict))
		// 细节行：功效警告、inconclusive 原因与最差 cell
		if v.Warning != "" {
			fmt.Fprintf(w, "%s└─ 警告: %s\n", indent(18), v.Warning)
		}
		if v.Note != "" && v.Verdict == "inconclusive" {
			fmt.Fprintf(w, "%s└─ %s\n", indent(18), v.Note)
		}
		if worst := worstCell(v); worst != nil {
			fmt.Fprintf(w, "%s└─ %s\n", indent(18), *worst)
		}
	}

	// 提示性 NOTE
	for _, n := range res.Notes {
		fmt.Fprintf(w, "\nNOTE  %s\n", n)
	}

	// 传输指标：描述性对比，不参与判定
	fmt.Fprintln(w)
	fmt.Fprintln(w, "transport (descriptive, not scored)")
	fmt.Fprintf(w, "  %-16s%s vs %s\n", "availability",
		fmtFloat(res.TransportA.Availability, 3), fmtFloat(res.TransportB.Availability, 3))
	printPct(w, "latency_ms p50", res.TransportA.LatencyMs.P50, res.TransportB.LatencyMs.P50)
	printPct(w, "ttft_ms p50", res.TransportA.TtftMs.P50, res.TransportB.TtftMs.P50)
	printPct(w, "tps p50", res.TransportA.Tps.P50, res.TransportB.Tps.P50)
}

// statisticText 统计量列文本：距离类印数值，p 值类印 p=，缺值印 "-"。
func statisticText(v probe.Verdict) string {
	if v.Statistic == nil {
		return "-"
	}
	if meta, ok := probe.Get(v.ProbeID); ok && meta.Meta().StatKind == probe.StatPValue {
		return "p=" + trimFloat(*v.Statistic)
	}
	switch v.ProbeID {
	case "onetoken":
		return trimFloat(*v.Statistic) + " JSD"
	}
	return trimFloat(*v.Statistic)
}

// thresholdText 阈值列文本：p 值类显示为 α=。
func thresholdText(v probe.Verdict) string {
	if v.Threshold <= 0 {
		return "-"
	}
	if meta, ok := probe.Get(v.ProbeID); ok && meta.Meta().StatKind == probe.StatPValue {
		return "α=" + trimFloat(v.Threshold)
	}
	return trimFloat(v.Threshold)
}

// worstCell 找出 fail 探针里最差的一个 cell 作为细节行。
func worstCell(v probe.Verdict) *string {
	if v.Verdict != "fail" || len(v.Cells) == 0 {
		return nil
	}
	// 距离类取 Stat 最大；p 值类（tokenizer）没有逐 cell p，取不匹配的 cell 摘要
	if meta, ok := probe.Get(v.ProbeID); ok && meta.Meta().StatKind == probe.StatPValue {
		var mismatched []string
		for _, c := range v.Cells {
			if m, _ := c.Extra["match"].(bool); !m && !c.Insufficient {
				mismatched = append(mismatched, cellKeyText(c))
			}
		}
		if len(mismatched) == 0 {
			return nil
		}
		shown := mismatched
		if len(shown) > 3 {
			shown = shown[:3]
		}
		s := fmt.Sprintf("mismatched cells (%d/%d): %s",
			len(mismatched), len(v.Cells), strings.Join(shown, ", "))
		return &s
	}
	worst, worstStat := -1, math.Inf(-1)
	for i, c := range v.Cells {
		if c.Insufficient || c.Stat == nil {
			continue
		}
		if *c.Stat > worstStat {
			worst, worstStat = i, *c.Stat
		}
	}
	if worst < 0 {
		return nil
	}
	c := v.Cells[worst]
	s := fmt.Sprintf("%s: jsd %s, n %d vs %d",
		cellKeyText(c), trimFloat(*c.Stat), c.NA, c.NB)
	return &s
}

// cellKeyText 把 cell 键渲染为 "k=v" 逗号串（键排序保证稳定）。
func cellKeyText(c probe.CellDetail) string {
	keys := make([]string, 0, len(c.Key))
	for k := range c.Key {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+c.Key[k])
	}
	return strings.Join(parts, " ")
}

func printPct(w io.Writer, label string, a, b *float64) {
	fmt.Fprintf(w, "  %-16s%s vs %s\n", label, fmtPtrFloat(a), fmtPtrFloat(b))
}

func fmtPtrFloat(v *float64) string {
	if v == nil {
		return "-"
	}
	return trimFloat(*v)
}

func fmtFloat(v float64, prec int) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.*f", prec, v), "0"), ".")
}

// trimFloat 去掉多余的尾零，保留至多 4 位小数。
func trimFloat(v float64) string {
	return fmtFloat(v, 4)
}

// truncateDigest 展示用截断："sha256:" + 6 位十六进制 + "…"，与 README 示例一致。
func truncateDigest(d string) string {
	const head = len("sha256:") + 6
	if len(d) > head {
		return d[:head] + "…"
	}
	return d
}

func indent(n int) string {
	return strings.Repeat(" ", n)
}
