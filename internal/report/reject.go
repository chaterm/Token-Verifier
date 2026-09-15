package report

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/chaterm/token-verifier/internal/compare"
)

// Reject 渲染 digest 闸门的拒绝信息：字段级 diff 按探针分组（DATAFLOW §3.2）。
// 子集模式（e.Subset）下的拒绝说明 strict 字段冲突或交集为空，措辞相应调整。
func Reject(w io.Writer, e *compare.GateError) {
	fmt.Fprintln(w, "ERROR  incompatible collection plans")
	fmt.Fprintf(w, "  plan_digest  %s  vs  %s\n", e.DigestA, e.DigestB)
	if e.FormatVersionA != e.FormatVersionB {
		fmt.Fprintf(w, "  format_version  %d  vs  %d\n", e.FormatVersionA, e.FormatVersionB)
	}
	fmt.Fprintln(w)

	// diff 按前缀分组：probes.<id>.* 归到该探针下，其余归入顶层
	probeGroups := map[string][]compare.FieldDiff{}
	var topLevel []compare.FieldDiff
	for _, d := range e.Diffs {
		if id, rest, ok := splitProbePath(d.Path); ok {
			d.Path = rest
			probeGroups[id] = append(probeGroups[id], d)
		} else {
			topLevel = append(topLevel, d)
		}
	}
	if len(topLevel) > 0 {
		fmt.Fprintln(w, "  plan")
		printDiffs(w, topLevel)
	}
	for _, id := range sortedKeys(probeGroups) {
		fmt.Fprintf(w, "\n  probe %s\n", id)
		printDiffs(w, probeGroups[id])
	}

	fmt.Fprintln(w)
	if e.Subset {
		fmt.Fprint(w, "子集模式（--allow-subset）下以上 strict 字段冲突仍不可调和；"+
			"或两侧计划没有可比较的公共部分。\n")
		return
	}
	fmt.Fprintf(w, "两侧必须使用同一 collection plan；本工具不做部分比较。\n")
	fmt.Fprintf(w, "如确认只需比较两侧计划的交集，可加 --allow-subset（strict 字段仍须一致）。\n")
}

// splitProbePath 把 "probes.onetoken.repeats" 拆成 ("onetoken", "repeats")。
func splitProbePath(path string) (id, rest string, ok bool) {
	trimmed := strings.TrimPrefix(path, ".")
	after, found := strings.CutPrefix(trimmed, "probes.")
	if !found {
		return "", "", false
	}
	id, rest, found = strings.Cut(after, ".")
	if !found {
		// 恰好是 probes.<id> 整个对象不同
		return id, "(whole block)", true
	}
	return id, rest, true
}

// printDiffs 以对齐的两列打印 diff。
func printDiffs(w io.Writer, diffs []compare.FieldDiff) {
	width := 0
	for _, d := range diffs {
		if len(d.Path) > width {
			width = len(d.Path)
		}
	}
	for _, d := range diffs {
		fmt.Fprintf(w, "    %-*s  %-14s vs  %s\n", width, d.Path, d.A, d.B)
	}
}

func sortedKeys(m map[string][]compare.FieldDiff) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
