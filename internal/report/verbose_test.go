package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/chaterm/token-verifier/internal/compare"
	"github.com/chaterm/token-verifier/internal/probe"
	"github.com/chaterm/token-verifier/internal/stats"
)

// verboseResult 含各类证据数据的结果：离散分布（onetoken）、
// 连续直方图（think-effort）、匹配明细（tokenizer）、传输直方图。
func verboseResult() *compare.Result {
	res := sampleResult()
	res.Verdicts[0].Cells[0].Extra = map[string]any{
		"a_dist": map[string]int{"7": 5, "3": 3, "5": 2},
		"b_dist": map[string]int{"7": 4, "3": 4, "9": 2},
	}
	res.Verdicts = append(res.Verdicts, probe.Verdict{
		ProbeID: "think-effort", Verdict: "pass",
		Statistic: f64(0.8), Threshold: 0.01, ThresholdSource: "config",
		Cells: []probe.CellDetail{{
			Key: map[string]string{"question_id": "te.v1.001", "context_bucket": "0",
				"thinking_effort": "high", "protocol": "openai-chat"},
			NA: 10, NB: 10, Stat: f64(0.8),
			Extra: map[string]any{
				"unit":     "tokens",
				"a_median": 512.0, "b_median": 505.0,
				"hist": stats.JointHist(
					[]float64{512, 480, 530, 495, 510}, []float64{505, 490, 520, 500, 515}, 4),
			},
		}},
	})
	res.Hists = compare.TransportHists{
		LatencyMs: stats.JointHist(
			[]float64{100, 110, 120}, []float64{500, 510, 520}, 4),
	}
	return res
}

func TestVerbosePerProbeEvidence(t *testing.T) {
	var buf bytes.Buffer
	Verbose(&buf, verboseResult())
	out := buf.String()

	for _, want := range []string{
		"onetoken",   // 探针段落标题
		"ot.v1.001",  // cell 键
		"n 20 vs 20", // 两侧样本量
		"7", "3",     // 分布取值
		"9",            // B 侧独有取值也要出现
		"think-effort", // 连续量探针
		"tokens",       // 单位
		"median",       // 中位数摘要
		"[",            // 直方图 bin 区间标记
		"latency_ms",   // 传输直方图段
		"A", "B",       // 两侧图例
	} {
		if !strings.Contains(out, want) {
			t.Errorf("verbose 输出缺少 %q:\n%s", want, out)
		}
	}
	// tokenizer fail cell 的两侧观测值明细
	for _, want := range []string{"128", "999", "tk.v1.001"} {
		if !strings.Contains(out, want) {
			t.Errorf("tokenizer 明细缺少 %q:\n%s", want, out)
		}
	}
}

func TestVerboseEmptyResult(t *testing.T) {
	// 无 cells、无直方图 → 不崩溃，输出段落标题即可
	var buf bytes.Buffer
	Verbose(&buf, &compare.Result{})
	if buf.Len() == 0 {
		t.Error("空结果也应输出段落框架")
	}
}

func TestBarScaling(t *testing.T) {
	// 0 → 空条；最大值 → 满宽；中间值按比例
	if got := bar(0, 10, 20); got != "" {
		t.Errorf("bar(0) = %q, want 空", got)
	}
	if got := bar(10, 10, 20); len([]rune(got)) != 20 {
		t.Errorf("bar(max) 宽度 = %d runes, want 20", len([]rune(got)))
	}
	half := bar(5, 10, 20)
	if len([]rune(half)) != 10 {
		t.Errorf("bar(5/10, 宽20) = %d runes, want 10", len([]rune(half)))
	}
	// maxN<=0 不崩溃
	if got := bar(3, 0, 20); got != "" {
		t.Errorf("bar(n, 0) = %q, want 空", got)
	}
}

func TestJSONCarriesEvidence(t *testing.T) {
	var buf bytes.Buffer
	if err := JSON(&buf, verboseResult()); err != nil {
		t.Fatal(err)
	}
	var rep map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}
	probes := rep["probes"].([]any)
	// onetoken cell 带 a_dist/b_dist
	ot := probes[0].(map[string]any)
	c0 := ot["cells"].([]any)[0].(map[string]any)
	if d, ok := c0["a_dist"].(map[string]any); !ok || d["7"] != float64(5) {
		t.Errorf("JSON cell a_dist = %v", c0["a_dist"])
	}
	// think-effort cell 带 hist（edges/a/b）
	te := probes[3].(map[string]any)
	tec := te["cells"].([]any)[0].(map[string]any)
	h, ok := tec["hist"].(map[string]any)
	if !ok {
		t.Fatalf("JSON cell hist = %v", tec["hist"])
	}
	if len(h["edges"].([]any)) != 5 || len(h["a"].([]any)) != 4 {
		t.Errorf("hist edges/a = %v / %v", h["edges"], h["a"])
	}
	// 顶层 transport 直方图
	th, ok := rep["transport_histograms"].(map[string]any)
	if !ok {
		t.Fatalf("JSON 缺 transport_histograms: %v", rep)
	}
	lat := th["latency_ms"].(map[string]any)
	if len(lat["edges"].([]any)) != 5 {
		t.Errorf("latency_ms hist edges = %v", lat["edges"])
	}
}

// TestVerboseStripsControlChars verbose 输出会原样回显服务端返回的答案取值
// 与工具名（onetoken dist / toolcall dist 的 key）。恶意端点可以在其中
// 塞 ANSI 转义序列伪造终端显示；渲染前必须剥掉 C0 控制字符与 ESC。
func TestVerboseStripsControlChars(t *testing.T) {
	res := sampleResult()
	res.Verdicts[0].Cells[0].Extra = map[string]any{
		"a_dist": map[string]int{"\x1b[31mfake\x07": 5},
		"b_dist": map[string]int{"ok": 5},
	}
	var buf bytes.Buffer
	Verbose(&buf, res)
	out := buf.String()
	if strings.ContainsRune(out, '\x1b') || strings.ContainsRune(out, '\x07') {
		t.Errorf("verbose 输出含未过滤的控制字符:\n%q", out)
	}
	if !strings.Contains(out, "fake") || !strings.Contains(out, "ok") {
		t.Errorf("过滤后仍应保留可见文本:\n%s", out)
	}
}
