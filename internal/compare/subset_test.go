package compare

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/chaterm/token-verifier/internal/config"
	"github.com/chaterm/token-verifier/internal/rawdata"
)

// subset_test.go：子集模式（--allow-subset）的调和、闸门与降采样。

// ---- fixture：可定制的探针计划块 ----

// probePlan 一个 onetoken 探针计划块的可定制构造器。
type probePlan struct {
	repeats        int
	temperature    any // float64 或 nil（→ 显式 null）
	topP           any
	maxTokens      any
	buckets        []int
	thinkingEffort any
	minN           int
	probeVersion   string
	cellKey        []string
	suiteVersion   string
	questionSetDig string // 缺省 sha256:qs
	questionIDs    []string
	extraProbe     string // 附加一个该 ID 的空探针块（模拟单侧独有探针）
}

func (p probePlan) build() map[string]any {
	ck := p.cellKey
	if ck == nil {
		ck = []string{"question_id", "context_bucket"}
	}
	ver := p.probeVersion
	if ver == "" {
		ver = "2"
	}
	suiteV := p.suiteVersion
	if suiteV == "" {
		suiteV = "v1"
	}
	qsd := p.questionSetDig
	if qsd == "" {
		qsd = "sha256:qs"
	}
	qids := p.questionIDs
	if qids == nil {
		qids = []string{"q1"}
	}
	block := map[string]any{
		"probe_version":      ver,
		"repeats":            p.repeats,
		"temperature":        p.temperature,
		"top_p":              p.topP,
		"max_tokens":         p.maxTokens,
		"thinking_effort":    p.thinkingEffort,
		"normalize_rule":     "suite/v1",
		"observation_schema": "onetoken/v2",
		"cell_key":           ck,
		"question_ids":       qids,
		"min_n":              p.minN,
	}
	if p.buckets != nil {
		block["context_buckets"] = p.buckets
	}
	probes := map[string]any{"onetoken": block}
	if p.extraProbe != "" {
		probes[p.extraProbe] = map[string]any{
			"probe_version": "1", "repeats": 1, "cell_key": ck,
			"observation_schema": "x/v1", "question_ids": []string{},
		}
	}
	return map[string]any{
		"suite_version":        suiteV,
		"question_set_digest":  qsd,
		"padding_algo_version": "pad/v1",
		"padding_seed":         42,
		"probes":               probes,
	}
}

func (p probePlan) file(t *testing.T, digest string, records []map[string]any) *rawdata.File {
	return rawFile{digest: digest, plan: p.build(), records: records}.build(t)
}

// ---- ReconcilePlans ----

func rawPlan(t *testing.T, p probePlan) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(p.build())
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestReconcileIdenticalPlans(t *testing.T) {
	p := probePlan{repeats: 10, buckets: []int{0, 8000}, temperature: 1.0}
	rec, conflicts, err := ReconcilePlans(rawPlan(t, p), rawPlan(t, p), "d1", "d1")
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("相同计划不应有冲突: %+v", conflicts)
	}
	if len(rec.ProbeIDs) != 1 || rec.Probes["onetoken"].Repeats != 10 {
		t.Errorf("rec = %+v", rec)
	}
	if len(rec.Excluded) != 0 {
		t.Errorf("不应有排除项: %+v", rec.Excluded)
	}
}

func TestReconcileStrictConflicts(t *testing.T) {
	base := probePlan{repeats: 10, temperature: 1.0, maxTokens: 16, buckets: []int{0}}
	cases := []struct {
		name  string
		mutB  func(*probePlan)
		field string
	}{
		{"temperature", func(p *probePlan) { p.temperature = 0.7 }, "temperature"},
		{"temperature null vs set", func(p *probePlan) { p.temperature = nil }, "temperature"},
		{"top_p", func(p *probePlan) { p.topP = 0.9 }, "top_p"},
		{"max_tokens", func(p *probePlan) { p.maxTokens = 32 }, "max_tokens"},
		{"thinking_effort", func(p *probePlan) { p.thinkingEffort = "high" }, "thinking_effort"},
		{"probe_version", func(p *probePlan) { p.probeVersion = "3" }, "probe_version"},
		{"cell_key", func(p *probePlan) { p.cellKey = []string{"question_id"} }, "cell_key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := base
			tc.mutB(&b)
			_, conflicts, err := ReconcilePlans(rawPlan(t, base), rawPlan(t, b), "d1", "d2")
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, d := range conflicts {
				if strings.HasSuffix(d.Path, "."+tc.field) {
					found = true
				}
			}
			if !found {
				t.Errorf("%s 差异应产生 strict 冲突: %+v", tc.field, conflicts)
			}
		})
	}
}

func TestReconcilePlanLevelStrict(t *testing.T) {
	// probes 之外的 plan 字段（suite_version）不同 → strict 冲突
	a := probePlan{repeats: 10}
	b := probePlan{repeats: 10, suiteVersion: "v2"}
	_, conflicts, err := ReconcilePlans(rawPlan(t, a), rawPlan(t, b), "d1", "d2")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range conflicts {
		if d.Path == "suite_version" {
			found = true
		}
	}
	if !found {
		t.Errorf("suite_version 差异应产生冲突: %+v", conflicts)
	}
}

func TestReconcileRelaxedAndIntersect(t *testing.T) {
	// repeats 不同 → 取小；min_n 不同 → 忽略；buckets 不同 → 交集
	a := probePlan{repeats: 20, minN: 10, buckets: []int{0, 8000, 32000}}
	b := probePlan{repeats: 12, minN: 5, buckets: []int{0, 32000, 131072}}
	rec, conflicts, err := ReconcilePlans(rawPlan(t, a), rawPlan(t, b), "d1", "d2")
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("repeats/min_n/buckets 差异不应产生冲突: %+v", conflicts)
	}
	pp := rec.Probes["onetoken"]
	if pp.Repeats != 12 {
		t.Errorf("repeats = %d, want 12（取小）", pp.Repeats)
	}
	want := []int{0, 32000}
	if len(pp.ContextBuckets) != 2 || pp.ContextBuckets[0] != want[0] || pp.ContextBuckets[1] != want[1] {
		t.Errorf("buckets = %v, want %v（交集升序）", pp.ContextBuckets, want)
	}
	// 排除项：A 独有 8000、B 独有 131072
	excl := map[int]string{}
	for _, ex := range rec.Excluded {
		if ex.Kind == "bucket" && ex.Bucket != nil {
			excl[*ex.Bucket] = ex.Reason
		}
	}
	if _, ok := excl[8000]; !ok {
		t.Errorf("应排除 A 独有档位 8000: %+v", rec.Excluded)
	}
	if _, ok := excl[131072]; !ok {
		t.Errorf("应排除 B 独有档位 131072: %+v", rec.Excluded)
	}
	// min_n 不一致应有说明性 NOTE（生效值由比较侧解析）
	hasMinNNote := false
	for _, n := range rec.Notes {
		if strings.Contains(n, "min_n") {
			hasMinNNote = true
		}
	}
	if !hasMinNNote {
		t.Errorf("min_n 不一致应有 NOTE: %v", rec.Notes)
	}
}

func TestReconcileBucketDisjointExcludesProbe(t *testing.T) {
	// 档位完全不相交 → 该探针剔除
	a := probePlan{repeats: 10, buckets: []int{8000}}
	b := probePlan{repeats: 10, buckets: []int{131072}}
	rec, conflicts, err := ReconcilePlans(rawPlan(t, a), rawPlan(t, b), "d1", "d2")
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("不应有 strict 冲突: %+v", conflicts)
	}
	if len(rec.ProbeIDs) != 0 {
		t.Errorf("档位无交集应剔除探针: %+v", rec.ProbeIDs)
	}
	hasProbeExcl := false
	for _, ex := range rec.Excluded {
		if ex.Kind == "probe" && strings.Contains(ex.Reason, "无交集") {
			hasProbeExcl = true
		}
	}
	if !hasProbeExcl {
		t.Errorf("应有「无交集」排除项: %+v", rec.Excluded)
	}
}

func TestReconcileSingleSidedProbes(t *testing.T) {
	// B 侧多一个探针 → 交集只含 onetoken，另一个进排除项
	a := probePlan{repeats: 10, buckets: []int{0}}
	b := probePlan{repeats: 10, buckets: []int{0}, extraProbe: "tokenizer"}
	rec, conflicts, err := ReconcilePlans(rawPlan(t, a), rawPlan(t, b), "d1", "d2")
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("单侧独有探针不应是 strict 冲突: %+v", conflicts)
	}
	if len(rec.ProbeIDs) != 1 || rec.ProbeIDs[0] != "onetoken" {
		t.Errorf("交集应只含 onetoken: %+v", rec.ProbeIDs)
	}
	found := false
	for _, ex := range rec.Excluded {
		if ex.Kind == "probe" && ex.ProbeID == "tokenizer" && strings.Contains(ex.Reason, "B 侧") {
			found = true
		}
	}
	if !found {
		t.Errorf("tokenizer 应进排除项: %+v", rec.Excluded)
	}
}

// ---- Gate：严格 vs 子集 ----

func TestGateStrictRejectsRelaxableDiff(t *testing.T) {
	// repeats 差异在严格模式下仍拒绝（默认行为不变）
	a := probePlan{repeats: 10, buckets: []int{0}}.file(t, "sha256:aa", nil)
	b := probePlan{repeats: 20, buckets: []int{0}}.file(t, "sha256:bb", nil)
	_, err := Gate(a, b, false)
	var gateErr *GateError
	if !errors.As(err, &gateErr) {
		t.Fatalf("严格模式应拒绝: %v", err)
	}
	if gateErr.Subset {
		t.Error("严格模式拒绝不应标 Subset")
	}
}

func TestGateSubsetAllowsRelaxable(t *testing.T) {
	a := probePlan{repeats: 10, buckets: []int{0}}.file(t, "sha256:aa", nil)
	b := probePlan{repeats: 20, buckets: []int{0}}.file(t, "sha256:bb", nil)
	rec, err := Gate(a, b, true)
	if err != nil {
		t.Fatalf("子集模式应放行 repeats 差异: %v", err)
	}
	if rec.Identical {
		t.Error("digest 不等时不应标 Identical")
	}
	if rec.Probes["onetoken"].Repeats != 10 {
		t.Errorf("repeats = %d, want 10（取小）", rec.Probes["onetoken"].Repeats)
	}
}

func TestGateSubsetStillRejectsStrict(t *testing.T) {
	a := probePlan{repeats: 10, temperature: 1.0}.file(t, "sha256:aa", nil)
	b := probePlan{repeats: 10, temperature: 0.7}.file(t, "sha256:bb", nil)
	_, err := Gate(a, b, true)
	var gateErr *GateError
	if !errors.As(err, &gateErr) || !gateErr.Subset {
		t.Fatalf("temperature 差异子集模式也应拒绝且标 Subset: %v", err)
	}
	if len(gateErr.Diffs) == 0 {
		t.Error("拒绝应带 strict 冲突 diff")
	}
}

func TestGateSubsetEmptyIntersection(t *testing.T) {
	// 两侧探针集合完全不相交 → 拒绝
	a := probePlan{repeats: 10, buckets: []int{0}, cellKey: []string{"question_id", "context_bucket"}}
	planA := a.build()
	delete(planA["probes"].(map[string]any), "onetoken")
	planA["probes"].(map[string]any)["tokenizer"] = map[string]any{
		"probe_version": "1", "repeats": 1, "cell_key": []string{"question_id"},
		"observation_schema": "tokenizer/v1", "question_ids": []string{},
	}
	b := probePlan{repeats: 10, buckets: []int{0}}
	fileA := rawFile{digest: "sha256:aa", plan: planA}.build(t)
	fileB := rawFile{digest: "sha256:bb", plan: b.build()}.build(t)
	_, err := Gate(fileA, fileB, true)
	var gateErr *GateError
	if !errors.As(err, &gateErr) || !gateErr.Subset {
		t.Fatalf("空交集应拒绝: %v", err)
	}
}

func TestGateFormatVersionAlwaysRejects(t *testing.T) {
	a := probePlan{repeats: 10}.file(t, "sha256:aa", nil)
	b := rawFile{digest: "sha256:bb", formatVersion: 2, plan: probePlan{repeats: 10}.build()}.build(t)
	if _, err := Gate(a, b, true); err == nil {
		t.Error("format_version 不等时子集模式也应拒绝")
	}
}

func TestCheckGateUnchangedStrict(t *testing.T) {
	// 旧入口 CheckGate 保持严格语义
	a := probePlan{repeats: 10}.file(t, "sha256:aa", nil)
	b := probePlan{repeats: 20}.file(t, "sha256:bb", nil)
	if err := CheckGate(a, b); err == nil {
		t.Error("CheckGate 应拒绝 digest 不等")
	}
}

// ---- RunWith：端到端子集比较 ----

func TestRunWithSubsetRepeatsDownsample(t *testing.T) {
	// A 侧 10 个 repeat（值全 "7"），B 侧 4 个（值全 "7"）。
	// 子集模式下按 repeat_index 降采样到 4，两侧同分布 → JSD=0 → pass；
	// NA/NB 应为降采样后的 4/4。
	var recsA []map[string]any
	for i := 0; i < 10; i++ {
		recsA = append(recsA, otRecord("q1", 0, "7", map[string]any{"repeat_index": i}))
	}
	var recsB []map[string]any
	for i := 0; i < 4; i++ {
		recsB = append(recsB, otRecord("q1", 0, "7", map[string]any{"repeat_index": i}))
	}
	a := probePlan{repeats: 10, buckets: []int{0}, minN: 3}.file(t, "sha256:aa", recsA)
	b := probePlan{repeats: 4, buckets: []int{0}, minN: 3}.file(t, "sha256:bb", recsB)

	res, err := RunWith(a, b, map[string]float64{"onetoken": 0.15}, nil, Options{AllowSubset: true})
	if err != nil {
		t.Fatal(err)
	}
	v := res.Verdicts[0]
	if v.Verdict != "pass" {
		t.Fatalf("verdict = %v (%s), want pass", v.Verdict, v.Note)
	}
	if v.Cells[0].NA != 4 || v.Cells[0].NB != 4 {
		t.Errorf("NA/NB = %d/%d, want 4/4（降采样到小侧）", v.Cells[0].NA, v.Cells[0].NB)
	}
	if res.Scope == nil {
		t.Fatal("子集模式应有 Scope")
	}
	if res.Scope.Repeats["onetoken"] != 4 {
		t.Errorf("Scope.Repeats = %v, want 4", res.Scope.Repeats)
	}
	// 默认 min_n（10）> 降采样后的 4 → 若 min_n 未随计划值 3 解析会 inconclusive；
	// 这里两侧计划都烘焙了 min_n=3，pass 即证明 min_n 走了计划值
}

func TestRunWithSubsetStrictReject(t *testing.T) {
	// temperature 不同 → 子集模式也拒绝
	a := probePlan{repeats: 10, temperature: 1.0}.file(t, "sha256:aa", nil)
	b := probePlan{repeats: 10, temperature: 0.7}.file(t, "sha256:bb", nil)
	_, err := RunWith(a, b, map[string]float64{"onetoken": 0.15}, nil, Options{AllowSubset: true})
	var gateErr *GateError
	if !errors.As(err, &gateErr) {
		t.Fatalf("err = %v, want *GateError", err)
	}
}

func TestRunWithSubsetBucketIntersection(t *testing.T) {
	// A: buckets [0, 8000]，B: [0]。交集 [0]：
	// 8000 的数据不参与统计且进排除项；阈值只要求 bucket 0。
	recs0A := mkOnetokenRecords("q1", 0, "7")
	recs8kA := mkOnetokenRecords("q1", 8000, "9") // 8000 档与 B 完全不同的分布
	recsA := append(append([]map[string]any{}, recs0A...), recs8kA...)
	recsB := mkOnetokenRecords("q1", 0, "7")

	a := probePlan{repeats: 10, buckets: []int{0, 8000}, minN: 10}.file(t, "sha256:aa", recsA)
	b := probePlan{repeats: 10, buckets: []int{0}, minN: 10}.file(t, "sha256:bb", recsB)

	// 只给探针级阈值：交集只剩一个档位，不应要求 8000 的档位阈值
	res, err := RunWith(a, b, map[string]float64{"onetoken": 0.15}, nil, Options{AllowSubset: true})
	if err != nil {
		t.Fatal(err)
	}
	v := res.Verdicts[0]
	if v.Verdict != "pass" {
		t.Fatalf("verdict = %v (%s), want pass（8000 档不该参与）", v.Verdict, v.Note)
	}
	for _, c := range v.Cells {
		if c.Key["context_bucket"] == "8000" {
			t.Errorf("8000 档的 cell 不应参与比较: %+v", c)
		}
	}
	found := false
	for _, ex := range res.Scope.Excluded {
		if ex.Kind == "bucket" && ex.Bucket != nil && *ex.Bucket == 8000 {
			found = true
		}
	}
	if !found {
		t.Errorf("8000 应进排除项: %+v", res.Scope.Excluded)
	}
	// NOTE 里应点名子集比较与排除的档位
	joined := strings.Join(res.Notes, "\n")
	if !strings.Contains(joined, "子集比较") || !strings.Contains(joined, "8000") {
		t.Errorf("Notes 应说明子集范围: %v", res.Notes)
	}
}

func TestRunWithSubsetThresholdOnlyForIntersection(t *testing.T) {
	// A: [0, 8000]，B: [0]。档位级阈值只给 onetoken.0：
	// 若阈值校验仍按 A 侧全量档位，会因缺 onetoken.8000 报错。
	recsA := mkOnetokenRecords("q1", 0, "7")
	recsB := mkOnetokenRecords("q1", 0, "7")
	a := probePlan{repeats: 10, buckets: []int{0, 8000}, minN: 10}.file(t, "sha256:aa", recsA)
	b := probePlan{repeats: 10, buckets: []int{0}, minN: 10}.file(t, "sha256:bb", recsB)

	res, err := RunWith(a, b, map[string]float64{"onetoken.0": 0.15}, nil, Options{AllowSubset: true})
	if err != nil {
		t.Fatalf("交集只剩 bucket 0，不应要求 8000 的阈值: %v", err)
	}
	if res.Verdicts[0].Verdict != "pass" {
		t.Errorf("verdict = %v, want pass", res.Verdicts[0].Verdict)
	}
}

func TestRunWithSubsetMinNFromLocalConfig(t *testing.T) {
	// 计划烘焙 min_n=10，每侧只有 5 个样本 → 默认 inconclusive；
	// 本地 config 覆盖 min_n=5 → pass。min_n 是判据参数，不参与闸门。
	var recs []map[string]any
	for i := 0; i < 5; i++ {
		recs = append(recs, otRecord("q1", 0, "7", map[string]any{"repeat_index": i}))
	}
	a := probePlan{repeats: 10, buckets: []int{0}, minN: 10}.file(t, "sha256:aa", recs)
	b := probePlan{repeats: 5, buckets: []int{0}, minN: 10}.file(t, "sha256:bb", recs)

	res, err := RunWith(a, b, map[string]float64{"onetoken": 0.15}, nil, Options{AllowSubset: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Verdicts[0].Verdict != "inconclusive" {
		t.Errorf("无 config 覆盖时应 inconclusive: %v", res.Verdicts[0].Verdict)
	}

	five := 5
	cfg := &config.File{Probes: map[string]config.ProbeConfig{"onetoken": {MinN: &five}}}
	res, err = RunWith(a, b, map[string]float64{"onetoken": 0.15}, cfg, Options{AllowSubset: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Verdicts[0].Verdict != "pass" {
		t.Errorf("config min_n=5 应可判: %v (%s)", res.Verdicts[0].Verdict, res.Verdicts[0].Note)
	}
	if res.Scope.MinN["onetoken"] != 5 {
		t.Errorf("Scope.MinN = %v, want 5（展示生效值）", res.Scope.MinN)
	}
}

func TestRunWithSubsetProbeIntersection(t *testing.T) {
	// A 只启用 onetoken，B 启用 onetoken+tokenizer → 交集只比 onetoken，
	// tokenizer 进排除项，Planned=1。
	planA := probePlan{repeats: 10, buckets: []int{0}, minN: 10}.build()
	planB := probePlan{repeats: 10, buckets: []int{0}, minN: 10, extraProbe: "tokenizer"}.build()
	recs := mkOnetokenRecords("q1", 0, "7")
	a := rawFile{digest: "sha256:aa", plan: planA, records: recs}.build(t)
	b := rawFile{digest: "sha256:bb", plan: planB, records: recs}.build(t)

	res, err := RunWith(a, b, map[string]float64{"onetoken": 0.15}, nil, Options{AllowSubset: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Planned != 1 || len(res.Verdicts) != 1 || res.Verdicts[0].ProbeID != "onetoken" {
		t.Fatalf("交集应只含 onetoken: planned=%d verdicts=%+v", res.Planned, res.Verdicts)
	}
	found := false
	for _, ex := range res.Scope.Excluded {
		if ex.ProbeID == "tokenizer" && ex.Kind == "probe" {
			found = true
		}
	}
	if !found {
		t.Errorf("tokenizer 应进排除项: %+v", res.Scope.Excluded)
	}
}

func TestRunWithStrictNoScope(t *testing.T) {
	// digest 相等的严格路径：Scope 为 nil，报告与旧版完全一致
	plan := probePlan{repeats: 10, buckets: []int{0}, minN: 10}.build()
	recs := mkOnetokenRecords("q1", 0, "7")
	a := rawFile{digest: "sha256:aa", plan: plan, records: recs}.build(t)
	b := rawFile{digest: "sha256:aa", plan: plan, records: recs}.build(t)

	res, err := Run(a, b, map[string]float64{"onetoken": 0.15}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Scope != nil {
		t.Errorf("严格路径不应有 Scope: %+v", res.Scope)
	}
	for _, n := range res.Notes {
		if strings.Contains(n, "子集") {
			t.Errorf("严格路径不应有子集 NOTE: %v", res.Notes)
		}
	}
}

// ---- 降采样确定性 ----

func TestDownsampleDeterministic(t *testing.T) {
	// 按 repeat_index 升序取前 n；乱序输入结果一致
	mk := func(repeats ...int) []cellItem {
		var items []cellItem
		for _, r := range repeats {
			raw, _ := json.Marshal(map[string]int{"v": r})
			items = append(items, cellItem{obs: raw, repeat: r})
		}
		return items
	}
	got1 := downsample(mk(2, 0, 3, 1), 2)
	got2 := downsample(mk(0, 1, 2, 3), 2)
	if len(got1) != 2 || len(got2) != 2 {
		t.Fatalf("长度 = %d/%d, want 2", len(got1), len(got2))
	}
	for i := range got1 {
		if string(got1[i]) != string(got2[i]) {
			t.Errorf("第 %d 条不一致: %s vs %s（应按 repeat_index 升序确定性截断）",
				i, got1[i], got2[i])
		}
	}
	// n 超过长度时全保留
	if got := downsample(mk(0, 1), 10); len(got) != 2 {
		t.Errorf("n>len 应全保留: %d", len(got))
	}
}

func TestReconcileQuestionSetDigestDerived(t *testing.T) {
	// question_set_digest 是派生字段（启用探针题目并集的哈希）：
	// 两侧启用探针集合不同（如一侧禁用 needle）时必然不等，
	// 子集模式不应因此拒绝 —— 真实题库差异由逐探针 question_ids 拦截。
	a := probePlan{repeats: 10, questionSetDig: "sha256:union-with-needle"}
	b := probePlan{repeats: 10, questionSetDig: "sha256:union-without-needle"}
	rec, conflicts, err := ReconcilePlans(rawPlan(t, a), rawPlan(t, b), "d1", "d2")
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 0 {
		t.Fatalf("question_set_digest 差异不应是 strict 冲突: %+v", conflicts)
	}
	if len(rec.ProbeIDs) != 1 {
		t.Errorf("探针应保留: %+v", rec.ProbeIDs)
	}
	hasNote := false
	for _, n := range rec.Notes {
		if strings.Contains(n, "question_set_digest") {
			hasNote = true
		}
	}
	if !hasNote {
		t.Errorf("应有说明性 NOTE: %v", rec.Notes)
	}
}

func TestReconcileQuestionIDsStillStrict(t *testing.T) {
	// 逐探针 question_ids 不同（真实题库差异）→ 仍拒绝
	a := probePlan{repeats: 10, questionIDs: []string{"q1"}}
	b := probePlan{repeats: 10, questionIDs: []string{"q1", "q2"}}
	_, conflicts, err := ReconcilePlans(rawPlan(t, a), rawPlan(t, b), "d1", "d2")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range conflicts {
		if strings.HasSuffix(d.Path, ".question_ids") {
			found = true
		}
	}
	if !found {
		t.Errorf("question_ids 差异应是 strict 冲突: %+v", conflicts)
	}
}
