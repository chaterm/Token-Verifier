package report

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"strings"
	"testing"

	"github.com/chaterm/token-verifier/internal/compare"
	"github.com/chaterm/token-verifier/internal/probe"
)

func f64(v float64) *float64 { return &v }

// sampleResult 构造一个含 pass/fail/inconclusive 三种判定的结果。
func sampleResult() *compare.Result {
	return &compare.Result{
		PlanDigest: "sha256:9f2c1a4b7d8e9f0123456789abcdef",
		Planned:    3,
		Compared:   2,
		Coverage:   2.0 / 3.0,
		Verdicts: []probe.Verdict{
			{
				ProbeID: "onetoken", Verdict: "pass",
				Statistic: f64(0.041), Threshold: 0.15, ThresholdSource: "config",
				Ratio: f64(0.27),
				Cells: []probe.CellDetail{
					{Key: map[string]string{"question_id": "ot.v1.001", "context_bucket": "0"},
						NA: 20, NB: 20, Stat: f64(0.041)},
				},
			},
			{
				ProbeID: "tokenizer", Verdict: "fail",
				Statistic: f64(0.004), Threshold: 0.01, ThresholdSource: "flag",
				Ratio: f64(2.5), Note: "method=fisher; matched 2/10 cells",
				Warning: "配对 cell 数 2 < 5，检验功效不足",
				Cells: []probe.CellDetail{
					{Key: map[string]string{"question_id": "tk.v1.001", "context_bucket": "0", "protocol": "openai-chat"},
						NA: 1, NB: 1, Stat: f64(0),
						Extra: map[string]any{"match": false, "a_values": []int{128}, "b_values": []int{999}}},
					{Key: map[string]string{"question_id": "tk.v1.002", "context_bucket": "0", "protocol": "openai-chat"},
						NA: 1, NB: 1, Stat: f64(1),
						Extra: map[string]any{"match": true}},
				},
			},
			{
				ProbeID: "needle", Verdict: "inconclusive",
				Note: "本版本尚未实现该探针",
			},
		},
		Notes: []string{"两侧的协议/传输分配（sampling）不同：观测差异可能来自请求格式而非模型本身"},
		TransportA: compare.TransportMetrics{
			Availability: 0.998, LatencyMs: compare.Percentiles{P50: f64(290)},
			TtftMs: compare.Percentiles{P50: f64(290)}, Tps: compare.Percentiles{P50: f64(45.1)},
		},
		TransportB: compare.TransportMetrics{
			Availability: 0.994, LatencyMs: compare.Percentiles{P50: f64(610)},
			TtftMs: compare.Percentiles{P50: f64(610)}, Tps: compare.Percentiles{P50: f64(22.7)},
		},
	}
}

func TestStdout(t *testing.T) {
	var buf bytes.Buffer
	Stdout(&buf, sampleResult())
	out := buf.String()

	for _, want := range []string{
		"plan_digest  sha256:9f2c1a…", // digest 截断
		"coverage     2/3 probes compared",
		"PROBE", "STATISTIC", "THRESHOLD", "SOURCE", "VERDICT",
		"onetoken", "0.041 JSD", "0.15", "config", "PASS",
		"tokenizer", "p=0.004", "α=0.01", "flag", "FAIL",
		"needle", "INCONCLUSIVE",
		"transport (descriptive, not scored)",
		"availability", "0.998 vs 0.994",
		"NOTE",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout 缺少 %q:\n%s", want, out)
		}
	}
	// fail 探针的细节行：mismatched cells
	if !strings.Contains(out, "mismatched cells") {
		t.Errorf("fail 探针应有 mismatched cells 细节行:\n%s", out)
	}
	// 功效警告行
	if !strings.Contains(out, "警告: 配对 cell 数 2 < 5") {
		t.Errorf("应渲染功效警告行:\n%s", out)
	}
}

func TestJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := JSON(&buf, sampleResult()); err != nil {
		t.Fatal(err)
	}
	var rep map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatalf("输出不是合法 JSON: %v\n%s", err, buf.String())
	}

	if rep["plan_digest"] != sampleResult().PlanDigest {
		t.Errorf("plan_digest = %v", rep["plan_digest"])
	}
	probes := rep["probes"].([]any)
	if len(probes) != 3 {
		t.Fatalf("probes = %d, want 3", len(probes))
	}
	tk := probes[1].(map[string]any)
	if tk["probe_id"] != "tokenizer" || tk["verdict"] != "fail" {
		t.Errorf("tokenizer probe = %v", tk)
	}
	if tk["threshold_source"] != "flag" || tk["statistic"].(float64) != 0.004 {
		t.Errorf("tokenizer 字段错误: %v", tk)
	}
	if tk["warning"] != "配对 cell 数 2 < 5，检验功效不足" {
		t.Errorf("tokenizer warning = %v", tk["warning"])
	}
	// cell 平铺：键分量 + a/b
	cells := tk["cells"].([]any)
	c0 := cells[0].(map[string]any)
	if c0["question_id"] != "tk.v1.001" || c0["protocol"] != "openai-chat" {
		t.Errorf("cell 键分量错误: %v", c0)
	}
	if a := c0["a"].(map[string]any); a["n"].(float64) != 1 {
		t.Errorf("cell.a = %v", c0["a"])
	}
	if c0["match"] != false {
		t.Errorf("cell.match = %v, want false", c0["match"])
	}
	// inconclusive 探针无 statistic（JSON null）
	nd := probes[2].(map[string]any)
	if nd["statistic"] != nil {
		t.Errorf("inconclusive statistic = %v, want null", nd["statistic"])
	}
	// transport 段存在
	if _, ok := rep["transport"].(map[string]any)["a"]; !ok {
		t.Error("JSON 缺 transport.a")
	}
}

func TestJUnit(t *testing.T) {
	var buf bytes.Buffer
	if err := JUnit(&buf, sampleResult()); err != nil {
		t.Fatal(err)
	}
	// 必须是合法 XML
	var suite struct {
		Name     string `xml:"name,attr"`
		Tests    int    `xml:"tests,attr"`
		Failures int    `xml:"failures,attr"`
		Skipped  int    `xml:"skipped,attr"`
		Cases    []struct {
			Name    string `xml:"name,attr"`
			Failure *struct {
				Message string `xml:"message,attr"`
			} `xml:"failure"`
			Skipped *struct {
				Message string `xml:"message,attr"`
			} `xml:"skipped"`
		} `xml:"testcase"`
	}
	if err := xml.Unmarshal(bytes.TrimPrefix(buf.Bytes(), []byte(xml.Header)), &suite); err != nil {
		t.Fatalf("输出不是合法 JUnit XML: %v\n%s", err, buf.String())
	}
	if suite.Tests != 3 || suite.Failures != 1 || suite.Skipped != 1 {
		t.Errorf("suite = %+v", suite)
	}
	if suite.Cases[1].Name != "tokenizer" || suite.Cases[1].Failure == nil {
		t.Errorf("tokenizer 应有 failure: %+v", suite.Cases[1])
	}
	if suite.Cases[2].Name != "needle" || suite.Cases[2].Skipped == nil {
		t.Errorf("needle 应有 skipped: %+v", suite.Cases[2])
	}
	if suite.Cases[0].Failure != nil || suite.Cases[0].Skipped != nil {
		t.Errorf("pass 的 testcase 不应有子元素: %+v", suite.Cases[0])
	}
}

func TestReject(t *testing.T) {
	var buf bytes.Buffer
	Reject(&buf, &compare.GateError{
		DigestA: "sha256:9f2c1a…", DigestB: "sha256:41ab7e…",
		Diffs: []compare.FieldDiff{
			{Path: "probes.onetoken.repeats", A: "20", B: "30"},
			{Path: "probes.onetoken.temperature", A: "1", B: "0.7"},
			{Path: "suite_version", A: "v1", B: "v2"},
		},
	})
	out := buf.String()
	for _, want := range []string{
		"ERROR  incompatible collection plans",
		"sha256:9f2c1a…", "sha256:41ab7e…",
		"probe onetoken",
		"repeats", "20", "30",
		"temperature", "0.7",
		"plan", "suite_version", "v1", "v2",
		"本工具不做部分比较",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("拒绝报告缺少 %q:\n%s", want, out)
		}
	}
}
