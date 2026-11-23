package compare

import (
	"strings"
	"testing"
)

// ---- 配对为空时的根因诊断 ----
//
// 探针层只能看到空配对，统一报「两侧没有共同 cell」；但空配对有两种截然不同的
// 根因：单侧没有成功样本（请求全失败/被能力裁剪跳过），或两侧都有样本但 cell 键
// 不相交。compare 层持有两侧原始 record，Note 必须区分到可操作的粒度。

// tePlan 生成含 think-effort 的计划（cell_key 与探针 Meta 同构）。
func tePlan(buckets ...int) map[string]any {
	return map[string]any{
		"suite_version": "v1",
		"probes": map[string]any{
			"think-effort": map[string]any{
				"probe_version": "2", "repeats": 10, "min_n": 5,
				"cell_key":        []string{"question_id", "context_bucket", "thinking_effort", "protocol"},
				"context_buckets": buckets,
				"question_ids":    []string{"q1"},
			},
		},
	}
}

// teRecord 一条 think-effort record（status 可控，observation 形状合法）。
func teRecord(bucket int, protocol, status string) map[string]any {
	return map[string]any{
		"probe_id": "think-effort", "question_id": "q1", "context_bucket": bucket,
		"protocol": protocol, "transport_mode": "stream", "thinking_effort": "high",
		"status": status, "observation": map[string]any{"reasoning_value": 512, "unit": "tokens"},
	}
}

func teRecords(n int, bucket int, protocol, status string) []map[string]any {
	out := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, teRecord(bucket, protocol, status))
	}
	return out
}

func TestRunNoPairsOneSideEmpty(t *testing.T) {
	// B 侧 think-effort 请求全部失败（0 条成功样本）→ 无法配对。
	// Note 应点名 B 侧缺数据，而不是笼统的「两侧没有共同 cell」。
	// 双档位走 compareBucketed rollup 路径。
	plan := tePlan(0, 8000)
	recsA := append(teRecords(10, 0, "openai-chat", "success"), teRecords(10, 8000, "openai-chat", "success")...)
	recsB := append(teRecords(10, 0, "openai-chat", "error"), teRecords(10, 8000, "openai-chat", "error")...)
	a := rawFile{digest: "sha256:aa", plan: plan, records: recsA}.build(t)
	b := rawFile{digest: "sha256:aa", plan: plan, records: recsB}.build(t)

	res, err := Run(a, b, map[string]float64{"think-effort": 0.01}, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := res.Verdicts[0]
	if v.Verdict != "inconclusive" {
		t.Fatalf("verdict = %s, want inconclusive", v.Verdict)
	}
	if !strings.Contains(v.Note, "B 侧无该探针的成功样本") {
		t.Errorf("note 应点名 B 侧缺成功样本, got: %s", v.Note)
	}
	if strings.Contains(v.Note, "两侧没有共同 cell") {
		t.Errorf("note 不应停留在症状层, got: %s", v.Note)
	}
}

func TestRunNoPairsBothSidesEmpty(t *testing.T) {
	// 两侧都没有成功样本（全部 error）→ 双侧缺数据。单档位路径。
	plan := tePlan(0)
	a := rawFile{digest: "sha256:aa", plan: plan, records: teRecords(10, 0, "openai-chat", "error")}.build(t)
	b := rawFile{digest: "sha256:aa", plan: plan, records: teRecords(10, 0, "openai-chat", "error")}.build(t)

	res, err := Run(a, b, map[string]float64{"think-effort": 0.01}, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := res.Verdicts[0]
	if v.Verdict != "inconclusive" {
		t.Fatalf("verdict = %s, want inconclusive", v.Verdict)
	}
	if !strings.Contains(v.Note, "两侧均无该探针的成功样本") {
		t.Errorf("note 应说明两侧均缺成功样本, got: %s", v.Note)
	}
}

func TestRunNoPairsKeysDisjoint(t *testing.T) {
	// 两侧都有成功样本，但协议不同 → cell 键不相交（cell_key 含 protocol）。
	// Note 应指向键不一致，而不是误报缺样本。
	plan := tePlan(0)
	a := rawFile{digest: "sha256:aa", plan: plan, records: teRecords(10, 0, "openai-chat", "success")}.build(t)
	b := rawFile{digest: "sha256:aa", plan: plan, records: teRecords(10, 0, "anthropic", "success")}.build(t)

	res, err := Run(a, b, map[string]float64{"think-effort": 0.01}, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := res.Verdicts[0]
	if v.Verdict != "inconclusive" {
		t.Fatalf("verdict = %s, want inconclusive", v.Verdict)
	}
	if !strings.Contains(v.Note, "cell 键无交集") {
		t.Errorf("note 应指向 cell 键不相交, got: %s", v.Note)
	}
}

func TestRunNoPairsAllFilteredByBuckets(t *testing.T) {
	// 子集模式防御角：两侧 record 都在交集档位之外（计划与数据不一致的 rawData）。
	// 配对本来非空、被档位过滤清空 → Note 不应误报「键无交集」或缺样本。
	planA := tePlan(0, 8000)
	planB := tePlan(0)
	a := rawFile{digest: "sha256:aa", plan: planA, records: teRecords(10, 8000, "openai-chat", "success")}.build(t)
	b := rawFile{digest: "sha256:bb", plan: planB, records: teRecords(10, 8000, "openai-chat", "success")}.build(t)

	res, err := RunWith(a, b, map[string]float64{"think-effort": 0.01}, nil, Options{AllowSubset: true})
	if err != nil {
		t.Fatal(err)
	}
	v := res.Verdicts[0]
	if v.Verdict != "inconclusive" {
		t.Fatalf("verdict = %s, want inconclusive", v.Verdict)
	}
	if !strings.Contains(v.Note, "交集档位之外") {
		t.Errorf("note 应说明配对被档位交集过滤清空, got: %s", v.Note)
	}
}

func TestRunPairedButInsufficientKeepsMinNNote(t *testing.T) {
	// 回归护栏：配对成功但样本不足（0 < n < min_n）仍走原有的准确提示，
	// 不被空配对诊断误伤。
	plan := tePlan(0)
	a := rawFile{digest: "sha256:aa", plan: plan, records: teRecords(3, 0, "openai-chat", "success")}.build(t)
	b := rawFile{digest: "sha256:aa", plan: plan, records: teRecords(3, 0, "openai-chat", "success")}.build(t)

	res, err := Run(a, b, map[string]float64{"think-effort": 0.01}, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := res.Verdicts[0]
	if v.Verdict != "inconclusive" {
		t.Fatalf("verdict = %s, want inconclusive", v.Verdict)
	}
	if !strings.Contains(v.Note, "有效样本量均不足") {
		t.Errorf("note 应保留样本不足提示, got: %s", v.Note)
	}
}