package report

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/chaterm/token-verifier/internal/compare"
	"github.com/chaterm/token-verifier/internal/probe"
	"github.com/chaterm/token-verifier/internal/stats"
)

// verbose.go：--verbose 的证据明细渲染。
// Stdout 回答「过没过」，Verbose 回答「凭什么」：逐 cell 印出两侧的
// 分布对比（离散取值直方图 / 连续量联合直方图）、匹配明细、召回率与
// 传输指标分布 —— 全部是描述性数据，与判定同源但不参与判定。

// verboseBarWidth ASCII 条形图的最大宽度（字符数）。
const verboseBarWidth = 20

// verboseMaxCells 每个探针最多渲染的 cell 数；超出部分提示看 JSON 报告
// （JSON 永远带全量 cells）。
const verboseMaxCells = 20

// Verbose 渲染逐探针的证据明细，接在 Stdout 报告之后输出。
func Verbose(w io.Writer, res *compare.Result) {
	fmt.Fprintln(w, "════ evidence detail ════")
	for _, v := range res.Verdicts {
		verboseProbe(w, v)
	}
	verboseTransport(w, res)
}

// verboseProbe 单探针段落：判定摘要 + 档位子标题 + 逐 cell 证据。
func verboseProbe(w io.Writer, v probe.Verdict) {
	source := ""
	if v.ThresholdSource != "" {
		source = " (" + v.ThresholdSource + ")"
	}
	fmt.Fprintf(w, "\n── %s  %s  statistic %s  threshold %s%s\n",
		v.ProbeID, strings.ToUpper(v.Verdict),
		statisticText(v.ProbeID, v.Statistic), thresholdText(v.ProbeID, v.Threshold), source)
	if v.Note != "" {
		fmt.Fprintf(w, "   note: %s\n", v.Note)
	}
	if len(v.Cells) == 0 {
		fmt.Fprintln(w, "   (无 cell 明细)")
		return
	}
	// 多档位：按档位分组渲染，每组一个小标题；cell 明细挂在各自档位下
	if len(v.Buckets) > 1 {
		byBucket := map[string][]probe.CellDetail{}
		for _, c := range v.Cells {
			byBucket[c.Key["context_bucket"]] = append(byBucket[c.Key["context_bucket"]], c)
		}
		shown := 0
		for _, bv := range v.Buckets {
			key := strconv.Itoa(bv.ContextBucket)
			cells := byBucket[key]
			if len(cells) == 0 {
				continue
			}
			bSource := ""
			if bv.ThresholdSource != "" {
				bSource = " (" + bv.ThresholdSource + ")"
			}
			fmt.Fprintf(w, "   [bucket %d] %s  statistic %s  threshold %s%s\n",
				bv.ContextBucket, strings.ToUpper(bv.Verdict),
				statisticText(v.ProbeID, bv.Statistic), thresholdText(v.ProbeID, bv.Threshold), bSource)
			for _, c := range cells {
				if shown >= verboseMaxCells {
					fmt.Fprintf(w, "   … 其余 %d 个 cell 见 JSON 报告\n", len(v.Cells)-verboseMaxCells)
					return
				}
				verboseCell(w, c)
				shown++
			}
		}
		return
	}
	for i, c := range v.Cells {
		if i >= verboseMaxCells {
			fmt.Fprintf(w, "   … 其余 %d 个 cell 见 JSON 报告\n", len(v.Cells)-verboseMaxCells)
			break
		}
		verboseCell(w, c)
	}
}

// verboseCell 单 cell 行 + 按探针类型渲染 Extra 里的证据数据。
func verboseCell(w io.Writer, c probe.CellDetail) {
	stat := "-"
	if c.Stat != nil {
		stat = trimFloat(*c.Stat)
	}
	suffix := ""
	if c.Insufficient {
		suffix = "  [insufficient]"
	}
	fmt.Fprintf(w, "   cell %s  n %d vs %d  stat %s%s\n",
		cellKeyText(c), c.NA, c.NB, stat, suffix)
	if c.Extra == nil {
		return
	}

	// 离散分布对比（onetoken 答案取值 / toolcall 工具选择）
	if aDist, bDist, ok := distPair(c.Extra); ok {
		renderDist(w, aDist, bDist)
	}
	// 连续量联合直方图（think-effort 思考量）
	if h, ok := c.Extra["hist"].(stats.Hist); ok {
		renderSummary(w, c.Extra)
		renderHist(w, h)
	}
	// tokenizer 两侧观测值明细
	if m, ok := c.Extra["match"].(bool); ok {
		label := "match"
		if !m {
			label = "MISMATCH"
		}
		fmt.Fprintf(w, "     %s", label)
		if a, ok := c.Extra["a_values"].([]int); ok {
			fmt.Fprintf(w, "  A prompt_tokens %v", a)
		}
		if b, ok := c.Extra["b_values"].([]int); ok {
			fmt.Fprintf(w, "  B prompt_tokens %v", b)
		}
		fmt.Fprintln(w)
	}
	// toolcall 参数合法率 / 并行率
	if av, ok := c.Extra["a_args_valid"].(int); ok {
		ai, _ := c.Extra["a_args_invalid"].(int)
		bv, _ := c.Extra["b_args_valid"].(int)
		bi, _ := c.Extra["b_args_invalid"].(int)
		fmt.Fprintf(w, "     args_valid: A %d/%d  B %d/%d", av, av+ai, bv, bv+bi)
		if ap, ok := c.Extra["a_parallel"].(int); ok {
			bp, _ := c.Extra["b_parallel"].(int)
			fmt.Fprintf(w, "   parallel: A %d  B %d", ap, bp)
		}
		fmt.Fprintln(w)
	}
	// needle 召回率
	if at, ok := c.Extra["a_total"].(int); ok {
		ah, _ := c.Extra["a_hit"].(int)
		bt, _ := c.Extra["b_total"].(int)
		bh, _ := c.Extra["b_hit"].(int)
		ar, _ := c.Extra["a_rate"].(float64)
		br, _ := c.Extra["b_rate"].(float64)
		fmt.Fprintf(w, "     recall: A %d/%d (%s)  B %d/%d (%s)\n",
			ah, at, trimFloat(ar), bh, bt, trimFloat(br))
	}
	// 解析失败提示（任何探针）
	if pf, ok := c.Extra["parse_failed"].(int); ok && pf > 0 {
		fmt.Fprintf(w, "     parse_failed: %d\n", pf)
	}
}

// distPair 提取 Extra 里的两侧离散分布。
func distPair(extra map[string]any) (map[string]int, map[string]int, bool) {
	a, okA := extra["a_dist"].(map[string]int)
	b, okB := extra["b_dist"].(map[string]int)
	return a, b, okA && okB
}

// renderSummary think-effort 的分位数摘要行。
func renderSummary(w io.Writer, extra map[string]any) {
	unit, _ := extra["unit"].(string)
	am, _ := extra["a_median"].(float64)
	bm, _ := extra["b_median"].(float64)
	aMean, _ := extra["a_mean"].(float64)
	bMean, _ := extra["b_mean"].(float64)
	fmt.Fprintf(w, "     summary (unit %s): A median %s mean %s | B median %s mean %s",
		unit, trimFloat(am), trimFloat(aMean), trimFloat(bm), trimFloat(bMean))
	if pv, ok := extra["p_value"].(float64); ok {
		fmt.Fprintf(w, " | p %s", trimFloat(pv))
	}
	fmt.Fprintln(w)
}

// renderDist 离散分布的两侧条形图：取值行对齐，A/B 各一条。
func renderDist(w io.Writer, a, b map[string]int) {
	keys := make(map[string]bool, len(a)+len(b))
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	maxN := 0
	for _, k := range sorted {
		maxN = max(maxN, a[k], b[k])
	}
	labelW := 10
	for _, k := range sorted {
		if len(k) > labelW {
			labelW = len(k)
		}
	}
	// 表头与数据行同列布局：label │ A 条 计数 │ B 条 计数
	fmt.Fprintf(w, "     %-*s A %-*s       B\n", labelW, "value", verboseBarWidth, "")
	for _, k := range sorted {
		// 取值 key 源自服务端返回的文本（onetoken 答案 / toolcall 工具名），
		// 渲染前剥控制字符 —— 恶意端点可以用 ANSI 转义序列伪造终端显示。
		fmt.Fprintf(w, "     %-*s A %-*s %3d   B %-*s %3d\n",
			labelW, sanitizeTerminal(k), verboseBarWidth, bar(a[k], maxN, verboseBarWidth), a[k],
			verboseBarWidth, bar(b[k], maxN, verboseBarWidth), b[k])
	}
}

// sanitizeTerminal 剥掉 C0 控制字符（含 ESC）与 DEL，替换为空格：
// 分布取值出现在对齐表格里，换行/制表同样会撕碎布局，一并替换。
func sanitizeTerminal(s string) string {
	if !strings.ContainsFunc(s, func(r rune) bool {
		return r < 0x20 || r == 0x7f
	}) {
		return s
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}

// renderHist 连续量联合直方图：每 bin 一行，区间 + 两侧条形。
// 两侧都为 0 的 bin 跳过（稀疏样本下大片空 bin 是噪声）；
// 区间标签本身保留了「哪里有缺口」的信息。
func renderHist(w io.Writer, h stats.Hist) {
	if h.Empty() {
		return
	}
	maxN := 0
	for i := range h.A {
		maxN = max(maxN, h.A[i], h.B[i])
	}
	for i := range h.A {
		if h.A[i] == 0 && h.B[i] == 0 {
			continue
		}
		closer := ")"
		if i == len(h.A)-1 {
			closer = "]" // 最后一个 bin 闭区间，最大值不丢
		}
		fmt.Fprintf(w, "     [%s,%s%s A %-*s %3d   B %-*s %3d\n",
			trimFloat(h.Edges[i]), trimFloat(h.Edges[i+1]), closer,
			verboseBarWidth, bar(h.A[i], maxN, verboseBarWidth), h.A[i],
			verboseBarWidth, bar(h.B[i], maxN, verboseBarWidth), h.B[i])
	}
}

// verboseTransport 传输指标的两侧联合直方图。
func verboseTransport(w io.Writer, res *compare.Result) {
	fmt.Fprintln(w, "\n── transport distributions (descriptive, not scored)")
	for _, m := range []struct {
		label string
		h     stats.Hist
	}{
		{"latency_ms", res.Hists.LatencyMs},
		{"ttft_ms", res.Hists.TtftMs},
		{"tps", res.Hists.Tps},
	} {
		fmt.Fprintf(w, "   %s\n", m.label)
		if m.h.Empty() {
			fmt.Fprintln(w, "     (无样本)")
			continue
		}
		renderHist(w, m.h)
	}
}

// bar 把 n/maxN 缩放到 width 个 '█'；n≤0 或 maxN≤0 返回空串，
// 非零 n 至少 1 格（避免小样本被抹成不可见）。
func bar(n, maxN, width int) string {
	if n <= 0 || maxN <= 0 {
		return ""
	}
	w := int(math.Round(float64(n) / float64(maxN) * float64(width)))
	if w < 1 {
		w = 1
	}
	if w > width {
		w = width
	}
	return strings.Repeat("█", w)
}
