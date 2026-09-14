package compare

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chaterm/token-verifier/internal/config"
	"github.com/chaterm/token-verifier/internal/rawdata"
)

// ---- fixture 构造 ----

// rawFile 一份 rawData 的构造参数。
type rawFile struct {
	digest        string
	formatVersion int
	plan          map[string]any
	sampling      map[string]any
	records       []map[string]any
	aggregates    bool
}

// build 写成 gzipped NDJSON 并读回，等价于走一遍真实文件路径。
func (f rawFile) build(t *testing.T) *rawdata.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "f.rawdata.jsonl.gz")
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := gzip.NewWriter(out)
	enc := json.NewEncoder(w)

	fv := f.formatVersion
	if fv == 0 {
		fv = 1
	}
	manifest := map[string]any{
		"kind":            "manifest",
		"format_version":  fv,
		"plan_digest":     f.digest,
		"collection_plan": f.plan,
	}
	if f.sampling != nil {
		manifest["sampling"] = f.sampling
	}
	if err := enc.Encode(manifest); err != nil {
		t.Fatal(err)
	}
	for _, r := range f.records {
		r["kind"] = "record"
		if err := enc.Encode(r); err != nil {
			t.Fatal(err)
		}
	}
	if f.aggregates {
		if err := enc.Encode(map[string]any{"kind": "aggregates", "record_count": len(f.records)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := rawdata.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// planWith 生成含 onetoken/tokenizer 计划的 plan。
func planWith(cellKeys map[string][]string) map[string]any {
	probes := map[string]any{}
	if ck, ok := cellKeys["onetoken"]; ok {
		probes["onetoken"] = map[string]any{"probe_version": "1", "repeats": 10, "cell_key": ck}
	}
	if ck, ok := cellKeys["tokenizer"]; ok {
		probes["tokenizer"] = map[string]any{"probe_version": "1", "repeats": 1, "cell_key": ck}
	}
	return map[string]any{"suite_version": "v1", "probes": probes}
}

// otRecord 一条 onetoken record。
func otRecord(qid string, bucket int, value string, extra ...map[string]any) map[string]any {
	r := map[string]any{
		"probe_id": "onetoken", "question_id": qid, "context_bucket": bucket,
		"protocol": "openai-chat", "transport_mode": "stream",
		"status": "success", "observation": map[string]any{"value": value},
	}
	for _, e := range extra {
		for k, v := range e {
			r[k] = v
		}
	}
	return r
}

// tkRecord 一条 tokenizer record。
func tkRecord(qid, protocol string, tokens int) map[string]any {
	return map[string]any{
		"probe_id": "tokenizer", "question_id": qid, "context_bucket": 0,
		"protocol": protocol, "transport_mode": "non_stream",
		"status": "success", "observation": map[string]any{"prompt_tokens": tokens},
	}
}

// mkOnetokenRecords 生成 n 条同题 record，取值按 values 循环。
func mkOnetokenRecords(qid string, bucket int, values ...string) []map[string]any {
	var out []map[string]any
	for i := 0; i < 10; i++ {
		out = append(out, otRecord(qid, bucket, values[i%len(values)]))
	}
	return out
}

// ---- 闸门 ----

func TestGateDigestMatch(t *testing.T) {
	plan := planWith(map[string][]string{"onetoken": {"question_id", "context_bucket"}})
	a := rawFile{digest: "sha256:aa", plan: plan, records: mkOnetokenRecords("q1", 0, "7")}.build(t)
	b := rawFile{digest: "sha256:aa", plan: plan, records: mkOnetokenRecords("q1", 0, "7")}.build(t)
	if err := CheckGate(a, b); err != nil {
		t.Errorf("相同 digest 应放行: %v", err)
	}
}

func TestGateDigestMismatch(t *testing.T) {
	planA := planWith(map[string][]string{"onetoken": {"question_id", "context_bucket"}})
	planB := planWith(map[string][]string{"onetoken": {"question_id", "context_bucket"}})
	// B 侧改了 repeats 与温度
	planB["probes"].(map[string]any)["onetoken"].(map[string]any)["repeats"] = 30
	planB["probes"].(map[string]any)["onetoken"].(map[string]any)["temperature"] = 0.7

	a := rawFile{digest: "sha256:aa", plan: planA, records: mkOnetokenRecords("q1", 0, "7")}.build(t)
	b := rawFile{digest: "sha256:bb", plan: planB, records: mkOnetokenRecords("q1", 0, "7")}.build(t)

	err := CheckGate(a, b)
	var gateErr *GateError
	if !errors.As(err, &gateErr) {
		t.Fatalf("err = %v, want *GateError", err)
	}
	// diff 应命中 probes.onetoken.repeats 与 temperature
	paths := map[string]FieldDiff{}
	for _, d := range gateErr.Diffs {
		paths[d.Path] = d
	}
	if d, ok := paths["probes.onetoken.repeats"]; !ok {
		t.Errorf("diff 应包含 repeats: %v", gateErr.Diffs)
	} else if d.A != "10" || d.B != "30" {
		t.Errorf("repeats diff = %q vs %q", d.A, d.B)
	}
	if d, ok := paths["probes.onetoken.temperature"]; !ok {
		t.Errorf("diff 应包含 temperature: %v", gateErr.Diffs)
	} else if d.A != absent || d.B != "0.7" {
		t.Errorf("temperature diff = %q vs %q", d.A, d.B)
	}
}

func TestGateFormatVersionMismatch(t *testing.T) {
	plan := planWith(map[string][]string{"onetoken": {"question_id", "context_bucket"}})
	a := rawFile{digest: "sha256:aa", formatVersion: 1, plan: plan}.build(t)
	b := rawFile{digest: "sha256:aa", formatVersion: 2, plan: plan}.build(t)
	err := CheckGate(a, b)
	var gateErr *GateError
	if !errors.As(err, &gateErr) || gateErr.FormatVersionA != 1 || gateErr.FormatVersionB != 2 {
		t.Errorf("err = %v, want format_version 不匹配", err)
	}
}

// ---- 编排 ----

func TestRunOnetokenPass(t *testing.T) {
	plan := planWith(map[string][]string{"onetoken": {"question_id", "context_bucket"}})
	// 两侧同分布 → JSD=0 → pass
	a := rawFile{digest: "sha256:aa", plan: plan, records: mkOnetokenRecords("q1", 0, "7", "3", "5"), aggregates: true}.build(t)
	b := rawFile{digest: "sha256:aa", plan: plan, records: mkOnetokenRecords("q1", 0, "7", "3", "5"), aggregates: true}.build(t)

	res, err := Run(a, b, map[string]float64{"onetoken": 0.15}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Verdicts) != 1 || res.Verdicts[0].Verdict != "pass" {
		t.Fatalf("verdicts = %+v", res.Verdicts)
	}
	if res.Compared != 1 || res.Planned != 1 || res.Coverage != 1 {
		t.Errorf("coverage = %v (%d/%d)", res.Coverage, res.Compared, res.Planned)
	}
	if res.Verdicts[0].ThresholdSource != "flag" {
		t.Errorf("source = %v, want flag", res.Verdicts[0].ThresholdSource)
	}
	if res.IncompleteA || res.IncompleteB {
		t.Error("两侧都有 aggregates，不应标 incomplete")
	}
}

func TestRunOnetokenFail(t *testing.T) {
	plan := planWith(map[string][]string{"onetoken": {"question_id", "context_bucket"}})
	a := rawFile{digest: "sha256:aa", plan: plan, records: mkOnetokenRecords("q1", 0, "7")}.build(t)
	b := rawFile{digest: "sha256:aa", plan: plan, records: mkOnetokenRecords("q1", 0, "9")}.build(t)

	res, err := Run(a, b, map[string]float64{"onetoken": 0.15}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Verdicts[0].Verdict != "fail" {
		t.Errorf("verdict = %v, want fail (分布完全不相交)", res.Verdicts[0].Verdict)
	}
}

func TestRunMissingThreshold(t *testing.T) {
	plan := planWith(map[string][]string{"onetoken": {"question_id", "context_bucket"}})
	a := rawFile{digest: "sha256:aa", plan: plan, records: mkOnetokenRecords("q1", 0, "7")}.build(t)
	b := rawFile{digest: "sha256:aa", plan: plan, records: mkOnetokenRecords("q1", 0, "7")}.build(t)

	// 无 flag、无 config → MissingThresholdError
	_, err := Run(a, b, nil, nil)
	var mte config.MissingThresholdError
	if !errors.As(err, &mte) || mte.ProbeID != "onetoken" {
		t.Errorf("err = %v, want MissingThresholdError(onetoken)", err)
	}

	// config 提供阈值即可通过
	cfg := &config.File{Thresholds: map[string]float64{"onetoken": 0.15}}
	res, err := Run(a, b, nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Verdicts[0].ThresholdSource != "config" {
		t.Errorf("source = %v, want config", res.Verdicts[0].ThresholdSource)
	}
}

func TestRunGateFirst(t *testing.T) {
	// 闸门在阈值之前：digest 不等时即使缺阈值也应报 GateError 而非缺阈值
	plan := planWith(map[string][]string{"onetoken": {"question_id", "context_bucket"}})
	a := rawFile{digest: "sha256:aa", plan: plan}.build(t)
	b := rawFile{digest: "sha256:bb", plan: plan}.build(t)
	_, err := Run(a, b, nil, nil)
	var gateErr *GateError
	if !errors.As(err, &gateErr) {
		t.Errorf("err = %v, want *GateError（闸门应先于阈值检查）", err)
	}
}

func TestRunTokenizerProtocolLayering(t *testing.T) {
	// tokenizer 的 cell_key 含 protocol：同一题在不同协议下是不同 cell。
	// 场景：4 cell 中仅 2 个不等 —— 证据不足以在 α=0.01 下判负（honest pass），
	// 本测试重点验证协议分层与逐 cell 匹配标记的正确性。
	plan := planWith(map[string][]string{"tokenizer": {"question_id", "context_bucket", "protocol"}})
	recsA := []map[string]any{
		tkRecord("t1", "openai-chat", 128),
		tkRecord("t1", "anthropic-messages", 130),
		tkRecord("t2", "openai-chat", 256),
		tkRecord("t2", "anthropic-messages", 260),
	}
	recsB := []map[string]any{
		tkRecord("t1", "openai-chat", 999),
		tkRecord("t1", "anthropic-messages", 130),
		tkRecord("t2", "openai-chat", 888),
		tkRecord("t2", "anthropic-messages", 260),
	}
	a := rawFile{digest: "sha256:aa", plan: plan, records: recsA}.build(t)
	b := rawFile{digest: "sha256:aa", plan: plan, records: recsB}.build(t)

	res, err := Run(a, b, map[string]float64{"tokenizer": 0.01}, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := res.Verdicts[0]
	if len(v.Cells) != 4 {
		t.Fatalf("cells = %d, want 4（2 题 × 2 协议）", len(v.Cells))
	}
	// 协议分层正确：anthropic 的 cell 应 match，openai-chat 的应不 match
	for _, c := range v.Cells {
		wantMatch := c.Key["protocol"] == "anthropic-messages"
		if m, _ := c.Extra["match"].(bool); m != wantMatch {
			t.Errorf("cell %+v match = %v, want %v", c.Key, m, wantMatch)
		}
	}
	if v.Verdict != "pass" {
		t.Errorf("2/4 cell 不等，p=%v，α=0.01 下应为 pass", *v.Statistic)
	}
}

func TestRunTokenizerDetectsFullMismatch(t *testing.T) {
	// 换分词器场景：单协议多题全部不等 → 小 p → fail
	plan := planWith(map[string][]string{"tokenizer": {"question_id", "context_bucket", "protocol"}})
	var recsA, recsB []map[string]any
	for i := 0; i < 8; i++ {
		qid := "t" + string(rune('a'+i))
		recsA = append(recsA, tkRecord(qid, "openai-chat", 100+i))
		recsB = append(recsB, tkRecord(qid, "openai-chat", 900+i))
	}
	a := rawFile{digest: "sha256:aa", plan: plan, records: recsA}.build(t)
	b := rawFile{digest: "sha256:aa", plan: plan, records: recsB}.build(t)

	res, err := Run(a, b, map[string]float64{"tokenizer": 0.01}, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := res.Verdicts[0]
	if v.Verdict != "fail" {
		t.Errorf("verdict = %v (p=%v), want fail", v.Verdict, *v.Statistic)
	}
	if *v.Statistic >= 0.01 {
		t.Errorf("p = %v, want < 0.01", *v.Statistic)
	}
}

func TestRunErrorRecordsExcluded(t *testing.T) {
	// status=error 的 record 不参与统计
	plan := planWith(map[string][]string{"onetoken": {"question_id", "context_bucket"}})
	recsA := mkOnetokenRecords("q1", 0, "7")
	recsA = append(recsA, otRecord("q1", 0, "999", map[string]any{"status": "error"}))
	a := rawFile{digest: "sha256:aa", plan: plan, records: recsA}.build(t)
	b := rawFile{digest: "sha256:aa", plan: plan, records: mkOnetokenRecords("q1", 0, "7")}.build(t)

	res, err := Run(a, b, map[string]float64{"onetoken": 0.15}, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := res.Verdicts[0]
	if v.Verdict != "pass" {
		t.Errorf("error record 应被排除，verdict = %v", v.Verdict)
	}
	if v.Cells[0].NA != 10 {
		t.Errorf("NA = %d, want 10（error 记录不计入）", v.Cells[0].NA)
	}
}

func TestRunCellsOnlyPairedBothSides(t *testing.T) {
	// 只有两侧都存在的 cell 才配对
	plan := planWith(map[string][]string{"onetoken": {"question_id", "context_bucket"}})
	recsA := append(mkOnetokenRecords("q1", 0, "7"), mkOnetokenRecords("q2", 0, "3")...)
	recsB := mkOnetokenRecords("q1", 0, "7") // B 侧只有 q1
	a := rawFile{digest: "sha256:aa", plan: plan, records: recsA}.build(t)
	b := rawFile{digest: "sha256:aa", plan: plan, records: recsB}.build(t)

	res, err := Run(a, b, map[string]float64{"onetoken": 0.15}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Verdicts[0].Cells) != 1 {
		t.Errorf("cells = %d, want 1（q2 只在 A 侧）", len(res.Verdicts[0].Cells))
	}
}

func TestRunRecordsNotMixedAcrossProbes(t *testing.T) {
	// 回归：onetoken 的 cell 里不得混入 tokenizer 的 record（分组必须按 probe_id 过滤）
	plan := planWith(map[string][]string{
		"onetoken":  {"question_id", "context_bucket"},
		"tokenizer": {"question_id", "context_bucket", "protocol"},
	})
	recsA := append(mkOnetokenRecords("q1", 0, "7"), tkRecord("t1", "openai-chat", 128))
	recsB := append(mkOnetokenRecords("q1", 0, "7"), tkRecord("t1", "openai-chat", 128))
	a := rawFile{digest: "sha256:aa", plan: plan, records: recsA}.build(t)
	b := rawFile{digest: "sha256:aa", plan: plan, records: recsB}.build(t)

	res, err := Run(a, b,
		map[string]float64{"onetoken": 0.15, "tokenizer": 0.01}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ot, tk := res.Verdicts[0], res.Verdicts[1]
	if ot.ProbeID != "onetoken" || len(ot.Cells) != 1 {
		t.Errorf("onetoken cells = %+v, want 恰好 1 个", ot.Cells)
	}
	if tk.ProbeID != "tokenizer" || len(tk.Cells) != 1 {
		t.Errorf("tokenizer cells = %+v, want 恰好 1 个", tk.Cells)
	}
}

func TestRunUnimplementedProbe(t *testing.T) {
	// 计划里有未实现的探针 → inconclusive，不要求阈值，coverage 降低
	// （needle 已实现，这里用虚构的未来探针 id；'f' < 'o' 保证排序在前）
	plan := planWith(map[string][]string{"onetoken": {"question_id", "context_bucket"}})
	plan["probes"].(map[string]any)["future-probe"] = map[string]any{"probe_version": "1", "cell_key": []string{"question_id", "context_bucket"}}
	a := rawFile{digest: "sha256:aa", plan: plan, records: mkOnetokenRecords("q1", 0, "7")}.build(t)
	b := rawFile{digest: "sha256:aa", plan: plan, records: mkOnetokenRecords("q1", 0, "7")}.build(t)

	res, err := Run(a, b, map[string]float64{"onetoken": 0.15}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Planned != 2 || res.Compared != 1 {
		t.Errorf("planned/compared = %d/%d, want 2/1", res.Planned, res.Compared)
	}
	// probeIDs 排序后 future-probe 在前
	var future = res.Verdicts[0]
	if future.ProbeID != "future-probe" {
		t.Errorf("第一个 verdict 应为 future-probe: %+v", res.Verdicts)
	}
	if future.Verdict != "inconclusive" {
		t.Errorf("future-probe verdict = %v, want inconclusive", future.Verdict)
	}
}

func TestRunSamplingNote(t *testing.T) {
	// 两侧 sampling 不同 → 提示性 NOTE，不拒绝
	plan := planWith(map[string][]string{"onetoken": {"question_id", "context_bucket"}})
	a := rawFile{digest: "sha256:aa", plan: plan,
		sampling: map[string]any{"onetoken": map[string]any{"openai-chat/stream": 10}},
		records:  mkOnetokenRecords("q1", 0, "7")}.build(t)
	b := rawFile{digest: "sha256:aa", plan: plan,
		sampling: map[string]any{"onetoken": map[string]any{"anthropic-messages/stream": 10}},
		records:  mkOnetokenRecords("q1", 0, "7")}.build(t)

	res, err := Run(a, b, map[string]float64{"onetoken": 0.15}, nil)
	if err != nil {
		t.Fatalf("sampling 差异不应拒绝: %v", err)
	}
	found := false
	for _, n := range res.Notes {
		if strings.Contains(n, "sampling") {
			found = true
		}
	}
	if !found {
		t.Errorf("应有 sampling NOTE: %v", res.Notes)
	}
}

func TestRunIncompleteSide(t *testing.T) {
	// 缺 aggregates → 标 incomplete，传输指标从 record 重算
	plan := planWith(map[string][]string{"onetoken": {"question_id", "context_bucket"}})
	a := rawFile{digest: "sha256:aa", plan: plan, records: mkOnetokenRecords("q1", 0, "7"), aggregates: true}.build(t)
	b := rawFile{digest: "sha256:aa", plan: plan, records: mkOnetokenRecords("q1", 0, "7")}.build(t)

	res, err := Run(a, b, map[string]float64{"onetoken": 0.15}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.IncompleteA || !res.IncompleteB {
		t.Errorf("incomplete = %v/%v, want false/true", res.IncompleteA, res.IncompleteB)
	}
	// 重算的传输指标：10 条 success record，availability=1
	if res.TransportB.Availability != 1 {
		t.Errorf("availability = %v, want 1", res.TransportB.Availability)
	}
	if res.TransportB.LatencyMs.P50 == nil {
		t.Error("latency p50 应从 record 重算")
	}
}

// ---- 档位（context_bucket）分区判定 ----

// planBuckets 生成含 context_buckets 的 onetoken 计划。
func planBuckets(buckets ...int) map[string]any {
	plan := planWith(map[string][]string{"onetoken": {"question_id", "context_bucket"}})
	plan["probes"].(map[string]any)["onetoken"].(map[string]any)["context_buckets"] = buckets
	return plan
}

func TestRunBucketPartitionWorstWins(t *testing.T) {
	// 两档位：bucket 0 两侧同分布（JSD=0），bucket 8000 完全不相交（JSD=1）。
	// 旧行为跨档位取均值 0.5 与单阈值比较；新行为逐档位判定，
	// rollup 取最差档位 → fail，且 Buckets 携带两条子判定。
	plan := planBuckets(0, 8000)
	recsA := append(mkOnetokenRecords("q1", 0, "7", "3", "5"), mkOnetokenRecords("q1", 8000, "7")...)
	recsB := append(mkOnetokenRecords("q1", 0, "7", "3", "5"), mkOnetokenRecords("q1", 8000, "9")...)
	a := rawFile{digest: "sha256:aa", plan: plan, records: recsA}.build(t)
	b := rawFile{digest: "sha256:aa", plan: plan, records: recsB}.build(t)

	res, err := Run(a, b, map[string]float64{"onetoken": 0.15}, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := res.Verdicts[0]
	if v.Verdict != "fail" {
		t.Fatalf("rollup = %v, want fail（最差档位 JSD=1 > 0.15）", v.Verdict)
	}
	if len(v.Buckets) != 2 {
		t.Fatalf("buckets = %+v, want 2 条子判定", v.Buckets)
	}
	b0, b8 := v.Buckets[0], v.Buckets[1]
	if b0.ContextBucket != 0 || b0.Verdict != "pass" {
		t.Errorf("bucket 0 = %+v, want pass", b0)
	}
	if b8.ContextBucket != 8000 || b8.Verdict != "fail" {
		t.Errorf("bucket 8000 = %+v, want fail", b8)
	}
	// rollup 的 Statistic/Threshold/Ratio 与最差档位一致
	if v.Statistic == nil || *v.Statistic != *b8.Statistic {
		t.Errorf("rollup statistic = %v, want 最差档位的 %v", v.Statistic, b8.Statistic)
	}
	if v.Threshold != b8.Threshold || v.Ratio == nil || *v.Ratio != *b8.Ratio {
		t.Errorf("rollup threshold/ratio 应取最差档位: %+v vs %+v", v, b8)
	}
	if *v.Ratio <= 1 {
		t.Errorf("fail 的 ratio 应 > 1: %v", *v.Ratio)
	}
	// cell 明细两档位都在
	if len(v.Cells) != 2 {
		t.Errorf("cells = %d, want 2", len(v.Cells))
	}
}

func TestRunBucketLevelThreshold(t *testing.T) {
	// 档位级阈值：bucket 0 用 0.15，bucket 8000 用 0.12。
	// 构造 JSD 落在 (0.12, 0.15) 之间困难，改用相反方向验证取值生效：
	// bucket 8000 完全不相交（JSD=1），档位阈值 1.5 > 1 → pass；
	// bucket 0 同分布（JSD=0）但档位阈值给个极小正数仍 pass。
	// 更直接的验证：让 bucket 8000 的档位阈值生效路径可观测 —— ThresholdSource。
	plan := planBuckets(0, 8000)
	recsA := append(mkOnetokenRecords("q1", 0, "7"), mkOnetokenRecords("q1", 8000, "7")...)
	recsB := append(mkOnetokenRecords("q1", 0, "9"), mkOnetokenRecords("q1", 8000, "7")...)
	a := rawFile{digest: "sha256:aa", plan: plan, records: recsA}.build(t)
	b := rawFile{digest: "sha256:aa", plan: plan, records: recsB}.build(t)

	cfg := &config.File{Thresholds: map[string]float64{
		"onetoken":      0.15,
		"onetoken.0":    1.5, // bucket 0 JSD=1 也 pass
		"onetoken.8000": 0.12,
	}}
	res, err := Run(a, b, nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	v := res.Verdicts[0]
	if len(v.Buckets) != 2 {
		t.Fatalf("buckets = %+v, want 2", v.Buckets)
	}
	b0, b8 := v.Buckets[0], v.Buckets[1]
	if b0.Threshold != 1.5 || b0.ThresholdSource != "config" || b0.Verdict != "pass" {
		t.Errorf("bucket 0 应取档位阈值 1.5 → pass: %+v", b0)
	}
	if b8.Threshold != 0.12 || b8.Verdict != "pass" {
		t.Errorf("bucket 8000 应取档位阈值 0.12 → pass（JSD=0）: %+v", b8)
	}
	// flag 档位键覆盖 config 档位键
	res, err = Run(a, b, map[string]float64{"onetoken.0": 0.5}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if th := res.Verdicts[0].Buckets[0]; th.Threshold != 0.5 || th.ThresholdSource != "flag" {
		t.Errorf("flag 档位键应覆盖 config: %+v", th)
	}
}

func TestRunBucketMissingThreshold(t *testing.T) {
	// 只给 bucket 0 的档位阈值、无探针级兜底 → bucket 8000 缺阈值报错
	plan := planBuckets(0, 8000)
	recsA := append(mkOnetokenRecords("q1", 0, "7"), mkOnetokenRecords("q1", 8000, "7")...)
	a := rawFile{digest: "sha256:aa", plan: plan, records: recsA}.build(t)
	b := rawFile{digest: "sha256:aa", plan: plan, records: recsA}.build(t)

	_, err := Run(a, b, map[string]float64{"onetoken.0": 0.15}, nil)
	var mte config.MissingThresholdError
	if !errors.As(err, &mte) || mte.ProbeID != "onetoken" || mte.Bucket == nil || *mte.Bucket != 8000 {
		t.Errorf("err = %v, want MissingThresholdError(onetoken, bucket 8000)", err)
	}
}

func TestRunBucketInconclusiveExcluded(t *testing.T) {
	// bucket 8000 样本不足（< min_n=10）→ 该档位 inconclusive，
	// 不污染整体：bucket 0 pass → rollup pass + Warning 点名。
	plan := planBuckets(0, 8000)
	recsA := append(mkOnetokenRecords("q1", 0, "7"),
		otRecord("q1", 8000, "7"), otRecord("q1", 8000, "3"))
	recsB := append(mkOnetokenRecords("q1", 0, "7"),
		otRecord("q1", 8000, "7"), otRecord("q1", 8000, "3"))
	a := rawFile{digest: "sha256:aa", plan: plan, records: recsA}.build(t)
	b := rawFile{digest: "sha256:aa", plan: plan, records: recsB}.build(t)

	res, err := Run(a, b, map[string]float64{"onetoken": 0.15}, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := res.Verdicts[0]
	if v.Verdict != "pass" {
		t.Fatalf("rollup = %v, want pass（inconclusive 档位排除）", v.Verdict)
	}
	if len(v.Buckets) != 2 || v.Buckets[1].Verdict != "inconclusive" {
		t.Errorf("bucket 8000 应 inconclusive: %+v", v.Buckets)
	}
	if !strings.Contains(v.Warning, "8000") {
		t.Errorf("Warning 应点名未判定档位: %q", v.Warning)
	}
	// coverage 仍按探针计：该探针已出结论
	if res.Compared != 1 {
		t.Errorf("compared = %d, want 1", res.Compared)
	}
}

func TestRunAllBucketsInconclusive(t *testing.T) {
	// 所有档位都样本不足 → 整体 inconclusive
	plan := planBuckets(0, 8000)
	recs := []map[string]any{otRecord("q1", 0, "7"), otRecord("q1", 8000, "7")}
	a := rawFile{digest: "sha256:aa", plan: plan, records: recs}.build(t)
	b := rawFile{digest: "sha256:aa", plan: plan, records: recs}.build(t)

	res, err := Run(a, b, map[string]float64{"onetoken": 0.15}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Verdicts[0].Verdict != "inconclusive" {
		t.Errorf("rollup = %v, want inconclusive", res.Verdicts[0].Verdict)
	}
	if res.Compared != 0 {
		t.Errorf("compared = %d, want 0", res.Compared)
	}
}

func TestRunSingleBucketNoSubVerdicts(t *testing.T) {
	// 单档位：不产生 Buckets 子判定，行为与旧版一致
	plan := planBuckets(0)
	a := rawFile{digest: "sha256:aa", plan: plan, records: mkOnetokenRecords("q1", 0, "7")}.build(t)
	b := rawFile{digest: "sha256:aa", plan: plan, records: mkOnetokenRecords("q1", 0, "7")}.build(t)

	res, err := Run(a, b, map[string]float64{"onetoken": 0.15}, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := res.Verdicts[0]
	if len(v.Buckets) != 0 {
		t.Errorf("单档位不应有子判定: %+v", v.Buckets)
	}
	if v.Verdict != "pass" || v.ThresholdSource != "flag" {
		t.Errorf("verdict = %+v, want pass/flag", v)
	}
	// 单档位时档位专用阈值也应生效
	res, err = Run(a, b, map[string]float64{"onetoken.0": 0.3}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if th := res.Verdicts[0].Threshold; th != 0.3 {
		t.Errorf("单档位专用阈值应生效: %v, want 0.3", th)
	}
}

func TestRunNoBucketCellKeyDegrades(t *testing.T) {
	// cell_key 不含 context_bucket（自定义计划）→ 不分区，探针级阈值
	plan := planWith(map[string][]string{"onetoken": {"question_id"}})
	a := rawFile{digest: "sha256:aa", plan: plan, records: mkOnetokenRecords("q1", 0, "7")}.build(t)
	b := rawFile{digest: "sha256:aa", plan: plan, records: mkOnetokenRecords("q1", 0, "7")}.build(t)

	res, err := Run(a, b, map[string]float64{"onetoken": 0.15}, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := res.Verdicts[0]
	if len(v.Buckets) != 0 || v.Verdict != "pass" {
		t.Errorf("退化路径应无子判定且 pass: %+v", v)
	}
}

func TestRunMinNFromPlan(t *testing.T) {
	// 每侧 5 个样本：plan 带 min_n=5 → 可判；缺省（回退 10）→ inconclusive
	var recs []map[string]any
	for i := 0; i < 5; i++ {
		recs = append(recs, otRecord("q1", 0, "7"))
	}

	plan := planWith(map[string][]string{"onetoken": {"question_id", "context_bucket"}})
	plan["probes"].(map[string]any)["onetoken"].(map[string]any)["min_n"] = 5
	a := rawFile{digest: "sha256:aa", plan: plan, records: recs, aggregates: true}.build(t)
	b := rawFile{digest: "sha256:aa", plan: plan, records: recs, aggregates: true}.build(t)
	res, err := Run(a, b, map[string]float64{"onetoken": 0.15}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Verdicts[0].Verdict != "pass" {
		t.Errorf("min_n=5 时 5 样本应可判: %v (%s)", res.Verdicts[0].Verdict, res.Verdicts[0].Note)
	}

	plan2 := planWith(map[string][]string{"onetoken": {"question_id", "context_bucket"}})
	a2 := rawFile{digest: "sha256:bb", plan: plan2, records: recs, aggregates: true}.build(t)
	b2 := rawFile{digest: "sha256:bb", plan: plan2, records: recs, aggregates: true}.build(t)
	res2, err := Run(a2, b2, map[string]float64{"onetoken": 0.15}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Verdicts[0].Verdict != "inconclusive" {
		t.Errorf("缺省 min_n=10 时 5 样本应 inconclusive: %v", res2.Verdicts[0].Verdict)
	}
}
