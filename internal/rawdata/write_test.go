package rawdata

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 写 manifest + 若干 record + aggregates，然后读回验证。
func TestWriteReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "out.rawdata.jsonl.gz")

	w, err := Append(p)
	if err != nil {
		t.Fatal(err)
	}
	m := Manifest{
		Kind:          "manifest",
		FormatVersion: FormatVersion,
		PlanDigest:    "sha256:aa",
	}
	b, _ := json.Marshal(m)
	if err := w.WriteLine(b); err != nil {
		t.Fatal(err)
	}
	httpCode := 200
	for i := 0; i < 3; i++ {
		rec := Record{
			ProbeID: "onetoken", QuestionID: "ot.1", ContextBucket: 0,
			Protocol: "openai-chat", TransportMode: "non_stream",
			RepeatIndex: i, Attempt: 0, Status: "success",
			HTTPCode: &httpCode, LatencyMs: 100,
			Observation: json.RawMessage(`{"value":"7"}`),
		}
		if err := w.WriteRecord(rec); err != nil {
			t.Fatal(err)
		}
	}
	// 多批：写完关再开（模拟分批落盘 / 崩溃重启）
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// 模拟崩溃：没有 aggregates 行
	f, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if f.Aggregates != nil {
		t.Fatal("未写 aggregates 前应为 nil（采集未完成标志）")
	}
	if len(f.Records) != 3 {
		t.Fatalf("records = %d", len(f.Records))
	}

	// 补写 aggregates
	if err := WriteAggregates(p, f.Records); err != nil {
		t.Fatal(err)
	}
	f2, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if f2.Aggregates == nil || f2.Aggregates.RecordCount != 3 {
		t.Fatalf("aggregates = %+v", f2.Aggregates)
	}
	ca := f2.Aggregates.Cells["onetoken|ot.1|0"]
	if ca.N != 3 || ca.NValid != 3 || ca.Distribution["7"] != 3 {
		t.Errorf("cell agg = %+v", ca)
	}
}

func TestEnsureFileNew(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "new.rawdata.jsonl.gz")
	m := Manifest{FormatVersion: 1, PlanDigest: "sha256:aa"}
	done, err := EnsureFile(p, m)
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 0 {
		t.Errorf("新文件 done 应为空")
	}
	// manifest 是第 1 行
	f, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if f.Manifest.PlanDigest != "sha256:aa" {
		t.Errorf("manifest = %+v", f.Manifest)
	}
}

func TestEnsureFileResumeDedup(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "out.rawdata.jsonl.gz")
	m := Manifest{FormatVersion: 1, PlanDigest: "sha256:aa"}
	if _, err := EnsureFile(p, m); err != nil {
		t.Fatal(err)
	}
	// 写一条成功 + 一条失败 record
	httpCode := 500
	errKind := "http_5xx"
	w, _ := Append(p)
	w.WriteRecord(Record{ProbeID: "onetoken", QuestionID: "ot.1", Protocol: "openai-chat",
		TransportMode: "non_stream", RepeatIndex: 0, Status: "success", HTTPCode: &httpCode, LatencyMs: 1})
	w.WriteRecord(Record{ProbeID: "onetoken", QuestionID: "ot.1", Protocol: "openai-chat",
		TransportMode: "non_stream", RepeatIndex: 1, Status: "error", HTTPCode: &httpCode,
		ErrorKind: &errKind, LatencyMs: 1})
	w.Close()

	done, err := EnsureFile(p, m)
	if err != nil {
		t.Fatal(err)
	}
	// 只有成功的 (repeat 0) 进 resume 集合；失败的 repeat 1 要重发
	if len(done) != 1 {
		t.Fatalf("done = %v, want 1 项", done)
	}
	key := ResumeKey{ProbeID: "onetoken", QuestionID: "ot.1", Protocol: "openai-chat", TransportMode: "non_stream", RepeatIndex: 0}
	if !done[key] {
		t.Errorf("done 缺少 %v", key)
	}
}

func TestEnsureFileDigestMismatch(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "out.rawdata.jsonl.gz")
	if _, err := EnsureFile(p, Manifest{PlanDigest: "sha256:aa"}); err != nil {
		t.Fatal(err)
	}
	_, err := EnsureFile(p, Manifest{PlanDigest: "sha256:bb"})
	if err == nil {
		t.Fatal("digest 不一致应拒绝续写")
	}
}

func TestEnsureFileStripsOldAggregates(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "out.rawdata.jsonl.gz")
	m := Manifest{PlanDigest: "sha256:aa"}
	if _, err := EnsureFile(p, m); err != nil {
		t.Fatal(err)
	}
	w, _ := Append(p)
	w.WriteRecord(Record{ProbeID: "onetoken", QuestionID: "ot.1", Protocol: "p",
		TransportMode: "non_stream", RepeatIndex: 0, Status: "success"})
	w.Close()
	f, _ := Read(p)
	if err := WriteAggregates(p, f.Records); err != nil {
		t.Fatal(err)
	}

	// 收官后再追加采集：EnsureFile 应去掉旧 aggregates 尾行
	done, err := EnsureFile(p, m)
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 1 {
		t.Fatalf("done = %v", done)
	}
	f2, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if f2.Aggregates != nil {
		t.Fatal("旧 aggregates 尾行应被去掉")
	}
	if len(f2.Records) != 1 {
		t.Fatalf("records = %d", len(f2.Records))
	}
}

func TestBuildAggregatesTransport(t *testing.T) {
	httpCode := 200
	mk := func(status, proto, mode string, latency int, ttft *int) Record {
		return Record{ProbeID: "onetoken", QuestionID: "ot.1", Protocol: proto,
			TransportMode: mode, RepeatIndex: 0, Status: status, HTTPCode: &httpCode,
			LatencyMs: latency, TtftMs: ttft}
	}
	ttft := 50
	records := []Record{
		mk("success", "openai-chat", "stream", 100, &ttft),
		mk("success", "openai-chat", "stream", 200, &ttft),
		mk("error", "openai-chat", "non_stream", 50, nil),
		mk("success", "anthropic-messages", "stream", 300, &ttft),
	}
	agg := BuildAggregates(records)
	if agg.Transport.Overall.Availability != 0.75 {
		t.Errorf("availability = %v, want 0.75", agg.Transport.Overall.Availability)
	}
	if agg.Transport.Overall.LatencyMs["p50"] == 0 {
		t.Errorf("latency p50 缺失")
	}
	if agg.Transport.ByProtocol["anthropic-messages"].Availability != 1 {
		t.Errorf("by_protocol = %+v", agg.Transport.ByProtocol)
	}
	if _, ok := agg.Transport.ByProtocolTransport["openai-chat|stream"]; !ok {
		t.Errorf("by_protocol_transport = %+v", agg.Transport.ByProtocolTransport)
	}
}

func TestTempFilePath(t *testing.T) {
	p, err := TempFilePath("tv")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); os.IsNotExist(err) {
		// ok：只要求路径可用
		t.Log(p)
	} else {
		t.Fatal("临时文件不应已存在")
	}
}

func TestCellKeyOfNewProbes(t *testing.T) {
	eff := "high"
	cases := []struct {
		rec  Record
		want string
	}{
		{Record{ProbeID: "onetoken", QuestionID: "q1", ContextBucket: 0}, "onetoken|q1|0"},
		{Record{ProbeID: "tokenizer", QuestionID: "q1", ContextBucket: 8000, Protocol: "openai-chat"},
			"tokenizer|q1|8000|openai-chat"},
		{Record{ProbeID: "think-effort", QuestionID: "te1", ContextBucket: 0, ThinkingEffort: &eff, Protocol: "openai-chat"},
			"think-effort|te1|0|high|openai-chat"},
		{Record{ProbeID: "think-effort", QuestionID: "te1", ContextBucket: 0, Protocol: "anthropic-messages"},
			"think-effort|te1|0|null|anthropic-messages"},
		{Record{ProbeID: "needle", QuestionID: "nd1", ContextBucket: 32000}, "needle|nd1|32000"},
		{Record{ProbeID: "toolcall", QuestionID: "tc1", ContextBucket: 0}, "toolcall|tc1|0"},
	}
	for _, c := range cases {
		if got := CellKeyOf(c.rec); got != c.want {
			t.Errorf("CellKeyOf(%s) = %q, want %q", c.rec.ProbeID, got, c.want)
		}
	}
}

func TestBuildAggregatesNewProbeSummaries(t *testing.T) {
	eff := "low"
	recs := []Record{
		{Kind: "record", ProbeID: "toolcall", QuestionID: "tc1", Status: "success",
			Observation: json.RawMessage(`{"tool":"get_weather","args_valid":true,"parallel":false,"num_calls":1}`)},
		{Kind: "record", ProbeID: "toolcall", QuestionID: "tc1", Status: "success",
			Observation: json.RawMessage(`{"tool":"get_weather","args_valid":false,"parallel":false,"num_calls":1}`)},
		{Kind: "record", ProbeID: "think-effort", QuestionID: "te1", Status: "success",
			ThinkingEffort: &eff, Protocol: "openai-chat",
			Observation: json.RawMessage(`{"reasoning_value":512,"unit":"tokens"}`)},
	}
	agg := BuildAggregates(recs)
	tc := agg.Cells["toolcall|tc1|0"]
	if tc.N != 2 || tc.NValid != 2 {
		t.Errorf("toolcall cell = %+v", tc)
	}
	if tc.Distribution["get_weather"] != 2 {
		t.Errorf("toolcall 分布 = %v, want get_weather:2", tc.Distribution)
	}
	te := agg.Cells["think-effort|te1|0|low|openai-chat"]
	if te.N != 1 || len(te.Values) != 1 || te.Values[0] != 512 {
		t.Errorf("think-effort cell = %+v, want values [512]", te)
	}
}

// Append 的文件错误同样不透 OS 原文，自带路径。
func TestAppendErrorMessagesIncludePath(t *testing.T) {
	badDir := filepath.Join(t.TempDir(), "nosuchdir", "out.gz")
	_, err := Append(badDir)
	if err == nil {
		t.Fatal("输出目录不存在应报错")
	}
	if !strings.Contains(err.Error(), "nosuchdir") {
		t.Errorf("错误应含路径: %v", err)
	}
	for _, leaked := range []string{"The system cannot find", "no such file", "open "} {
		if strings.Contains(err.Error(), leaked) {
			t.Errorf("不应泄露 OS 原文 %q: %v", leaked, err)
		}
	}
}
