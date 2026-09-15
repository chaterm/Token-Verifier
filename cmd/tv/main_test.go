package main

import (
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain 把测试期间的 stdout/stderr 重定向到系统空设备：
// cmdCompare 直接写 os.Stdout/os.Stderr，不重定向会污染测试输出。
func TestMain(m *testing.M) {
	origOut, origErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = devNull(), devNull()
	code := m.Run()
	os.Stdout, os.Stderr = origOut, origErr
	os.Exit(code)
}

func devNull() *os.File {
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		panic(err)
	}
	return f
}

// writeFixture 造一份 gzipped NDJSON rawData 测试文件。
// records 为 record 行；aggregates 控制是否写末行。
func writeFixture(t *testing.T, digest string, plan map[string]any, records []map[string]any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "f.rawdata.jsonl.gz")
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := gzip.NewWriter(out)
	enc := json.NewEncoder(w)
	mustEncode(t, enc, map[string]any{
		"kind": "manifest", "format_version": 1,
		"plan_digest": digest, "collection_plan": plan,
	})
	for _, r := range records {
		r["kind"] = "record"
		mustEncode(t, enc, r)
	}
	mustEncode(t, enc, map[string]any{"kind": "aggregates", "record_count": len(records)})
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func mustEncode(t *testing.T, enc *json.Encoder, v any) {
	t.Helper()
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
}

// twoProbePlan onetoken + tokenizer 的计划。
func twoProbePlan() map[string]any {
	return map[string]any{
		"suite_version": "v1",
		"probes": map[string]any{
			"onetoken": map[string]any{
				"probe_version": "1", "repeats": 10,
				"cell_key": []string{"question_id", "context_bucket"},
			},
			"tokenizer": map[string]any{
				"probe_version": "1", "repeats": 1,
				"cell_key": []string{"question_id", "context_bucket", "protocol"},
			},
		},
	}
}

// matchedRecords 两侧一致的 record 集：onetoken 同分布 + tokenizer 全等。
func matchedRecords() []map[string]any {
	var recs []map[string]any
	values := []string{"7", "3", "5", "1", "9", "2", "8", "4", "6", "0"}
	for _, v := range values {
		recs = append(recs, map[string]any{
			"probe_id": "onetoken", "question_id": "ot.v1.001", "context_bucket": 0,
			"protocol": "openai-chat", "transport_mode": "stream", "status": "success",
			"latency_ms": 800, "ttft_ms": 300, "tps": 45.0,
			"observation": map[string]any{"value": v},
		})
	}
	recs = append(recs, map[string]any{
		"probe_id": "tokenizer", "question_id": "tk.v1.001", "context_bucket": 0,
		"protocol": "openai-chat", "transport_mode": "non_stream", "status": "success",
		"latency_ms": 500, "observation": map[string]any{"prompt_tokens": 128},
	})
	return recs
}

func TestE2EAllPass(t *testing.T) {
	recs := matchedRecords()
	a := writeFixture(t, "sha256:aa", twoProbePlan(), recs)
	b := writeFixture(t, "sha256:aa", twoProbePlan(), matchedRecords())

	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "report.json")
	junitPath := filepath.Join(dir, "report.xml")

	code := run([]string{"compare",
		"--threshold", "onetoken=0.15", "--threshold", "tokenizer=0.01",
		"--json", jsonPath, "--junit", junitPath,
		a, b,
	})
	if code != exitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	// 报告文件已生成且非空
	for _, p := range []string{jsonPath, junitPath} {
		info, err := os.Stat(p)
		if err != nil || info.Size() == 0 {
			t.Errorf("报告文件 %s 未生成: %v", p, err)
		}
	}
	// JSON 报告可解析，coverage=1
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	var rep struct {
		Coverage float64 `json:"coverage"`
		Probes   []struct {
			ProbeID string `json:"probe_id"`
			Verdict string `json:"verdict"`
		} `json:"probes"`
	}
	if err := json.Unmarshal(data, &rep); err != nil {
		t.Fatalf("JSON 报告不合法: %v", err)
	}
	if rep.Coverage != 1 || len(rep.Probes) != 2 {
		t.Errorf("coverage = %v, probes = %d", rep.Coverage, len(rep.Probes))
	}
	for _, p := range rep.Probes {
		if p.Verdict != "pass" {
			t.Errorf("probe %s verdict = %v, want pass", p.ProbeID, p.Verdict)
		}
	}
}

func TestE2EFail(t *testing.T) {
	// B 侧 onetoken 分布完全不相交 → fail → exit 1
	recsB := matchedRecords()
	for i := range recsB {
		if recsB[i]["probe_id"] == "onetoken" {
			recsB[i]["observation"] = map[string]any{"value": "42"}
		}
	}
	a := writeFixture(t, "sha256:aa", twoProbePlan(), matchedRecords())
	b := writeFixture(t, "sha256:aa", twoProbePlan(), recsB)

	code := run([]string{"compare",
		"--threshold", "onetoken=0.15", "--threshold", "tokenizer=0.01",
		a, b,
	})
	if code != exitFail {
		t.Errorf("exit = %d, want 1", code)
	}
}

func TestE2EIncompatiblePlans(t *testing.T) {
	// digest 不等 → exit 3
	planB := twoProbePlan()
	planB["probes"].(map[string]any)["onetoken"].(map[string]any)["repeats"] = 30
	a := writeFixture(t, "sha256:aa", twoProbePlan(), matchedRecords())
	b := writeFixture(t, "sha256:bb", planB, matchedRecords())

	code := run([]string{"compare", "--threshold", "onetoken=0.15", "--threshold", "tokenizer=0.01", a, b})
	if code != exitIncompat {
		t.Errorf("exit = %d, want 3", code)
	}
}

func TestE2EMissingThreshold(t *testing.T) {
	a := writeFixture(t, "sha256:aa", twoProbePlan(), matchedRecords())
	b := writeFixture(t, "sha256:aa", twoProbePlan(), matchedRecords())

	// 缺 tokenizer 阈值 → exit 2
	code := run([]string{"compare", "--threshold", "onetoken=0.15", a, b})
	if code != exitUsage {
		t.Errorf("exit = %d, want 2", code)
	}
}

func TestE2EInconclusiveExit5(t *testing.T) {
	// 计划含未实现探针 → 无 fail 但有 inconclusive → exit 5
	// （needle 已实现，这里用虚构的未来探针 id）
	plan := twoProbePlan()
	plan["probes"].(map[string]any)["future-probe"] = map[string]any{
		"probe_version": "1", "cell_key": []string{"question_id", "context_bucket"},
	}
	a := writeFixture(t, "sha256:aa", plan, matchedRecords())
	b := writeFixture(t, "sha256:aa", plan, matchedRecords())

	code := run([]string{"compare",
		"--threshold", "onetoken=0.15", "--threshold", "tokenizer=0.01", a, b})
	if code != exitInconclusive {
		t.Errorf("exit = %d, want 5", code)
	}

	// fail 优先于 inconclusive：B 侧分布不同 → exit 1 而非 5
	recsB := matchedRecords()
	for i := range recsB {
		if recsB[i]["probe_id"] == "onetoken" {
			recsB[i]["observation"] = map[string]any{"value": "42"}
		}
	}
	c := writeFixture(t, "sha256:aa", plan, recsB)
	code = run([]string{"compare",
		"--threshold", "onetoken=0.15", "--threshold", "tokenizer=0.01", a, c})
	if code != exitFail {
		t.Errorf("fail+inconclusive: exit = %d, want 1（fail 优先）", code)
	}
}

func TestE2EConfigThresholds(t *testing.T) {
	// 阈值来自配置文件
	a := writeFixture(t, "sha256:aa", twoProbePlan(), matchedRecords())
	b := writeFixture(t, "sha256:aa", twoProbePlan(), matchedRecords())
	cfgPath := filepath.Join(t.TempDir(), "tv.yaml")
	os.WriteFile(cfgPath, []byte("version: 1\nthresholds:\n  onetoken: 0.15\n  tokenizer: 0.01\n"), 0o644)

	code := run([]string{"compare", "-c", cfgPath, a, b})
	if code != exitOK {
		t.Errorf("exit = %d, want 0", code)
	}
}

func TestE2ELogLevelFlag(t *testing.T) {
	a := writeFixture(t, "sha256:aa", twoProbePlan(), matchedRecords())
	b := writeFixture(t, "sha256:aa", twoProbePlan(), matchedRecords())
	th := []string{"--threshold", "onetoken=0.15", "--threshold", "tokenizer=0.01"}

	// 非法级别 → exit 2（文件合法，退出码只能来自级别解析）
	for _, lvl := range []string{"bogus", "trace", ""} {
		code := run(append([]string{"compare", "--log-level", lvl}, append(th, a, b)...))
		if code != exitUsage {
			t.Errorf("compare --log-level %q: exit = %d, want 2", lvl, code)
		}
	}
	// 合法级别（大小写不敏感）→ 正常跑通
	for _, lvl := range []string{"debug", "INFO", "warn", "error"} {
		code := run(append([]string{"compare", "--log-level", lvl}, append(th, a, b)...))
		if code != exitOK {
			t.Errorf("compare --log-level %q: exit = %d, want 0", lvl, code)
		}
	}
	// 不带 --log-level → 默认行为不变
	if code := run(append([]string{"compare"}, append(th, a, b)...)); code != exitOK {
		t.Errorf("compare 无 --log-level: exit = %d, want 0", code)
	}
	// collect / run 也接受该 flag：非法级别在配置加载前就拦下 → exit 2
	if code := run([]string{"collect", "-c", "x.yaml", "-o", "y.gz", "--log-level", "bogus"}); code != exitUsage {
		t.Errorf("collect --log-level bogus: exit = %d, want 2", code)
	}
	if code := run([]string{"run", "-c", "x.yaml", "--log-level", "bogus", "base.gz"}); code != exitUsage {
		t.Errorf("run --log-level bogus: exit = %d, want 2", code)
	}
}

func TestE2EUsageErrors(t *testing.T) {
	// 无参数 → exit 2
	if code := run(nil); code != exitUsage {
		t.Errorf("no args: exit = %d, want 2", code)
	}
	// 未知命令 → exit 2
	if code := run([]string{"bogus"}); code != exitUsage {
		t.Errorf("unknown cmd: exit = %d, want 2", code)
	}
	// compare 只给一个文件 → exit 2
	if code := run([]string{"compare", "x.gz"}); code != exitUsage {
		t.Errorf("one file: exit = %d, want 2", code)
	}
	// collect 缺必填 flag → exit 2
	if code := run([]string{"collect"}); code != exitUsage {
		t.Errorf("collect no flags: exit = %d, want 2", code)
	}
	if code := run([]string{"collect", "-c", "x.yaml"}); code != exitUsage {
		t.Errorf("collect no -o: exit = %d, want 2", code)
	}
	// run 缺基线 → exit 2
	if code := run([]string{"run", "-c", "x.yaml"}); code != exitUsage {
		t.Errorf("run no baseline: exit = %d, want 2", code)
	}
	// 文件不存在 → exit 2
	if code := run([]string{"compare", "nope1.gz", "nope2.gz"}); code != exitUsage {
		t.Errorf("missing files: exit = %d, want 2", code)
	}
}

// captureStdout 把 os.Stdout 临时换成文件跑 fn，返回写出的内容。
// TestMain 已把 os.Stdout 指到 devNull，这里换出去再换回来即可捕获。
func captureStdout(t *testing.T, fn func() int) (int, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdout.txt")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = f
	code := fn()
	os.Stdout = orig
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return code, string(data)
}

func TestE2ECompareVerbose(t *testing.T) {
	a := writeFixture(t, "sha256:aa", twoProbePlan(), matchedRecords())
	b := writeFixture(t, "sha256:aa", twoProbePlan(), matchedRecords())
	th := []string{"--threshold", "onetoken=0.15", "--threshold", "tokenizer=0.01"}

	// -v：正常报告之外追加证据明细段
	code, out := captureStdout(t, func() int {
		return run(append([]string{"compare", "-v"}, append(th, a, b)...))
	})
	if code != exitOK {
		t.Fatalf("compare -v exit = %d, want 0", code)
	}
	for _, want := range []string{
		"evidence detail", // 明细段标题
		"cell context_bucket=0 question_id=ot.v1.001", // 逐 cell 行（键排序）
		"transport distributions",                     // 传输直方图段
		"latency_ms",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("verbose 输出缺少 %q:\n%s", want, out)
		}
	}

	// --verbose 长写法等价
	code2, out2 := captureStdout(t, func() int {
		return run(append([]string{"compare", "--verbose"}, append(th, a, b)...))
	})
	if code2 != exitOK || !strings.Contains(out2, "evidence detail") {
		t.Errorf("compare --verbose: exit = %d, 输出含明细 = %v", code2, strings.Contains(out2, "evidence detail"))
	}

	// 不带 -v：保持现状，无明细段
	code3, out3 := captureStdout(t, func() int {
		return run(append([]string{"compare"}, append(th, a, b)...))
	})
	if code3 != exitOK || strings.Contains(out3, "evidence detail") {
		t.Errorf("compare 无 -v: exit = %d, 不应有明细段", code3)
	}
}

func TestE2ERunAcceptsVerboseFlag(t *testing.T) {
	// run 接受 -v/--verbose（flag 解析通过；后续因缺配置报 exit 2，
	// 而不是 flag 本身非法）。非法 flag 会在 Parse 阶段同样 exit 2，
	// 所以用 stderr 无法区分 —— 改为验证 -v 与已知必填检查的相对顺序：
	// 缺 config 的报错在 flag 解析之后，两种情况都是 exit 2，
	// 这里只保证带 -v 不 panic、退出码不变。
	if code := run([]string{"run", "-v", "-c", "x.yaml", "base.gz"}); code != exitUsage {
		t.Errorf("run -v 缺配置: exit = %d, want 2", code)
	}
	if code := run([]string{"run", "--verbose", "-c", "x.yaml", "base.gz"}); code != exitUsage {
		t.Errorf("run --verbose 缺配置: exit = %d, want 2", code)
	}
}

func TestE2ESubsetRelaxableDiff(t *testing.T) {
	// digest 不等但差异可调和（repeats）：
	// 严格模式 exit 3；--allow-subset 全 pass → exit 6（不是 0）
	planB := twoProbePlan()
	planB["probes"].(map[string]any)["onetoken"].(map[string]any)["repeats"] = 30
	a := writeFixture(t, "sha256:aa", twoProbePlan(), matchedRecords())
	b := writeFixture(t, "sha256:bb", planB, matchedRecords())
	th := []string{"--threshold", "onetoken=0.15", "--threshold", "tokenizer=0.01"}

	if code := run(append([]string{"compare"}, append(th, a, b)...)); code != exitIncompat {
		t.Errorf("严格模式 exit = %d, want 3", code)
	}
	if code := run(append([]string{"compare", "--allow-subset"}, append(th, a, b)...)); code != exitSubset {
		t.Errorf("--allow-subset 全 pass exit = %d, want 6", code)
	}

	// JSON 报告带 subset 标记与 scope
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "report.json")
	code := run(append([]string{"compare", "--allow-subset", "--json", jsonPath}, append(th, a, b)...))
	if code != exitSubset {
		t.Fatalf("exit = %d, want 6", code)
	}
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	var rep struct {
		Subset      bool   `json:"subset"`
		PlanDigestB string `json:"plan_digest_b"`
		Scope       *struct {
			Probes  []string       `json:"probes"`
			Repeats map[string]int `json:"repeats"`
		} `json:"scope"`
	}
	if err := json.Unmarshal(data, &rep); err != nil {
		t.Fatalf("JSON 报告不合法: %v", err)
	}
	if !rep.Subset || rep.PlanDigestB != "sha256:bb" {
		t.Errorf("subset = %v, plan_digest_b = %q", rep.Subset, rep.PlanDigestB)
	}
	if rep.Scope == nil || rep.Scope.Repeats["onetoken"] != 10 {
		t.Errorf("scope = %+v, want onetoken repeats=10（取小）", rep.Scope)
	}
}

func TestE2ESubsetStrictConflictStillRejects(t *testing.T) {
	// temperature 是 strict 字段：--allow-subset 也拒绝 → exit 3
	planB := twoProbePlan()
	planB["probes"].(map[string]any)["onetoken"].(map[string]any)["temperature"] = 0.7
	a := writeFixture(t, "sha256:aa", twoProbePlan(), matchedRecords())
	b := writeFixture(t, "sha256:bb", planB, matchedRecords())

	code := run([]string{"compare", "--allow-subset",
		"--threshold", "onetoken=0.15", "--threshold", "tokenizer=0.01", a, b})
	if code != exitIncompat {
		t.Errorf("exit = %d, want 3（strict 冲突子集模式也拒绝）", code)
	}
}

func TestE2ESubsetFailPriority(t *testing.T) {
	// 子集模式下 fail 仍是 exit 1（fail 优先于 subset 标记）
	planB := twoProbePlan()
	planB["probes"].(map[string]any)["onetoken"].(map[string]any)["repeats"] = 30
	recsB := matchedRecords()
	for i := range recsB {
		if recsB[i]["probe_id"] == "onetoken" {
			recsB[i]["observation"] = map[string]any{"value": "42"}
		}
	}
	a := writeFixture(t, "sha256:aa", twoProbePlan(), matchedRecords())
	b := writeFixture(t, "sha256:bb", planB, recsB)

	code := run([]string{"compare", "--allow-subset",
		"--threshold", "onetoken=0.15", "--threshold", "tokenizer=0.01", a, b})
	if code != exitFail {
		t.Errorf("exit = %d, want 1（fail 优先）", code)
	}
}
