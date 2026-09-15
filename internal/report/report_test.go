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

// bucketedResult 在 sampleResult 基础上给 onetoken 挂两条档位子判定：
// bucket 0 pass，bucket 8000 fail；rollup 取最差（8000）。
func bucketedResult() *compare.Result {
	res := sampleResult()
	res.Verdicts[0] = probe.Verdict{
		ProbeID: "onetoken", Verdict: "fail",
		Statistic: f64(0.17), Threshold: 0.12, ThresholdSource: "config",
		Ratio: f64(1.42), Note: "2 个档位各自判定，取最差（bucket 8000）",
		Buckets: []probe.BucketVerdict{
			{ContextBucket: 0, Verdict: "pass", Statistic: f64(0.083),
				Threshold: 0.1, ThresholdSource: "config", Ratio: f64(0.83)},
			{ContextBucket: 8000, Verdict: "fail", Statistic: f64(0.17),
				Threshold: 0.12, ThresholdSource: "flag", Ratio: f64(1.42)},
		},
		Cells: []probe.CellDetail{
			{Key: map[string]string{"question_id": "ot.v1.001", "context_bucket": "0"},
				NA: 20, NB: 20, Stat: f64(0.083)},
			{Key: map[string]string{"question_id": "ot.v1.001", "context_bucket": "8000"},
				NA: 20, NB: 20, Stat: f64(0.17)},
		},
	}
	return res
}

func TestStdoutBuckets(t *testing.T) {
	var buf bytes.Buffer
	Stdout(&buf, bucketedResult())
	out := buf.String()
	for _, want := range []string{
		"bucket 0", "bucket 8000", // 档位子行
		"0.083 JSD", "0.17 JSD", // 各档位统计量
		"├─", "└─", // 分支符号
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout 缺少 %q:\n%s", want, out)
		}
	}
}

func TestJSONBuckets(t *testing.T) {
	var buf bytes.Buffer
	if err := JSON(&buf, bucketedResult()); err != nil {
		t.Fatal(err)
	}
	var rep map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	ot := rep["probes"].([]any)[0].(map[string]any)
	buckets, ok := ot["buckets"].([]any)
	if !ok || len(buckets) != 2 {
		t.Fatalf("buckets = %v, want 2 条", ot["buckets"])
	}
	b8 := buckets[1].(map[string]any)
	if b8["context_bucket"].(float64) != 8000 || b8["verdict"] != "fail" ||
		b8["threshold"].(float64) != 0.12 || b8["threshold_source"] != "flag" {
		t.Errorf("bucket 8000 = %v", b8)
	}
	// 单档位探针（sampleResult 的 tokenizer）不输出 buckets 键
	tk := rep["probes"].([]any)[1].(map[string]any)
	if _, has := tk["buckets"]; has {
		t.Errorf("单档位不应有 buckets 键: %v", tk)
	}
}

func TestJUnitBuckets(t *testing.T) {
	var buf bytes.Buffer
	if err := JUnit(&buf, bucketedResult()); err != nil {
		t.Fatal(err)
	}
	var suite struct {
		Tests    int `xml:"tests,attr"`
		Failures int `xml:"failures,attr"`
		Skipped  int `xml:"skipped,attr"`
		Cases    []struct {
			Name    string    `xml:"name,attr"`
			Failure *struct{} `xml:"failure"`
		} `xml:"testcase"`
	}
	if err := xml.Unmarshal(bytes.TrimPrefix(buf.Bytes(), []byte(xml.Header)), &suite); err != nil {
		t.Fatal(err)
	}
	// onetoken 两档位 + tokenizer + needle = 4 个 testcase
	if suite.Tests != 4 || suite.Failures != 2 || suite.Skipped != 1 {
		t.Errorf("suite = %+v, want tests 4 / failures 2 / skipped 1", suite)
	}
	if suite.Cases[0].Name != "onetoken[context_bucket=0]" || suite.Cases[0].Failure != nil {
		t.Errorf("case 0 = %+v", suite.Cases[0])
	}
	if suite.Cases[1].Name != "onetoken[context_bucket=8000]" || suite.Cases[1].Failure == nil {
		t.Errorf("case 1 = %+v, want fail", suite.Cases[1])
	}
}

func TestVerboseBuckets(t *testing.T) {
	var buf bytes.Buffer
	Verbose(&buf, bucketedResult())
	out := buf.String()
	for _, want := range []string{
		"[bucket 0] PASS", "[bucket 8000] FAIL",
		"threshold 0.1", "threshold 0.12",
		"(flag)", // 档位阈值来源
	} {
		if !strings.Contains(out, want) {
			t.Errorf("verbose 缺少 %q:\n%s", want, out)
		}
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

func TestStdoutSubsetScope(t *testing.T) {
	// 子集模式：比较范围强制印在报告顶部
	bucket := 8000
	res := sampleResult()
	res.Scope = &compare.Scope{
		Mode:    "subset",
		DigestA: "sha256:9f2c1a4b7d8e9f0123456789abcdef",
		DigestB: "sha256:41ab7e0000000000000000000000ff",
		Probes:  []string{"onetoken", "tokenizer"},
		Repeats: map[string]int{"onetoken": 10, "tokenizer": 1},
		MinN:    map[string]int{"onetoken": 5},
		Excluded: []compare.ScopeExclusion{
			{Kind: "bucket", ProbeID: "onetoken", Bucket: &bucket, Reason: "仅 A 侧计划包含该档位"},
			{Kind: "probe", ProbeID: "needle", Reason: "仅 B 侧计划包含该探针"},
		},
	}
	var buf bytes.Buffer
	Stdout(&buf, res)
	out := buf.String()

	for _, want := range []string{
		"SCOPE        subset comparison (--allow-subset)",
		"sha256:9f2c1a…", "sha256:41ab7e…",
		"onetoken, tokenizer",
		"repeats=10", "min_n=5",
		"onetoken/context_bucket=8000",
		"needle — 仅 B 侧计划包含该探针",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout 缺少 %q:\n%s", want, out)
		}
	}
	// 子集模式下不再印 "(match)"
	if strings.Contains(out, "(match)") {
		t.Errorf("子集模式不应印 digest match:\n%s", out)
	}
}

func TestJSONSubsetScope(t *testing.T) {
	res := sampleResult()
	res.Scope = &compare.Scope{
		Mode: "subset", DigestA: "sha256:aa", DigestB: "sha256:bb",
		Probes: []string{"onetoken"}, Repeats: map[string]int{"onetoken": 10},
	}
	var buf bytes.Buffer
	if err := JSON(&buf, res); err != nil {
		t.Fatal(err)
	}
	var rep map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rep["subset"] != true || rep["plan_digest_b"] != "sha256:bb" {
		t.Errorf("subset/plan_digest_b = %v/%v", rep["subset"], rep["plan_digest_b"])
	}
	if rep["scope"] == nil {
		t.Error("应有 scope 段")
	}
	// 严格模式（无 Scope）不输出 subset 字段
	buf.Reset()
	if err := JSON(&buf, sampleResult()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), `"subset"`) {
		t.Errorf("严格模式 JSON 不应含 subset:\n%s", buf.String())
	}
}

func TestJUnitSubsetProperties(t *testing.T) {
	bucket := 8000
	res := sampleResult()
	res.Scope = &compare.Scope{
		Mode: "subset", DigestA: "sha256:aa", DigestB: "sha256:bb",
		Probes: []string{"onetoken"},
		Excluded: []compare.ScopeExclusion{
			{Kind: "bucket", ProbeID: "onetoken", Bucket: &bucket, Reason: "仅 A 侧"},
		},
	}
	var buf bytes.Buffer
	if err := JUnit(&buf, res); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		`name="subset" value="true"`,
		`name="plan_digest_b" value="sha256:bb"`,
		`name="scope.probes" value="onetoken"`,
		`name="scope.excluded.onetoken.bucket_8000"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("JUnit 缺少 %q:\n%s", want, out)
		}
	}
}

func TestRejectSubset(t *testing.T) {
	var buf bytes.Buffer
	Reject(&buf, &compare.GateError{
		DigestA: "sha256:aa", DigestB: "sha256:bb", Subset: true,
		Diffs: []compare.FieldDiff{
			{Path: "probes.onetoken.temperature", A: "1", B: "0.7"},
		},
	})
	out := buf.String()
	if !strings.Contains(out, "strict 字段冲突仍不可调和") {
		t.Errorf("子集拒绝应有专属措辞:\n%s", out)
	}
	if strings.Contains(out, "本工具不做部分比较") {
		t.Errorf("子集拒绝不应印严格模式措辞:\n%s", out)
	}

	// 严格模式拒绝应提示 --allow-subset 的存在
	buf.Reset()
	Reject(&buf, &compare.GateError{DigestA: "sha256:aa", DigestB: "sha256:bb"})
	if !strings.Contains(buf.String(), "--allow-subset") {
		t.Errorf("严格拒绝应提示 --allow-subset:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "本工具不做部分比较") {
		t.Errorf("严格拒绝措辞应保持:\n%s", buf.String())
	}
}
