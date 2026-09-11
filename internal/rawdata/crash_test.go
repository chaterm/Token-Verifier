package rawdata

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 模拟采集崩溃：gzip 成员写到一半进程死亡 → 文件尾部是不完整的 deflate 流。
// 读回应保留已完成的行并标记 Truncated，而不是整个文件报错。
func TestReadTruncatedGzip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "crashed.rawdata.jsonl.gz")

	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	manifest := `{"kind":"manifest","format_version":1,"plan_digest":"sha256:aa","collection_plan":{}}`
	rec := `{"kind":"record","probe_id":"onetoken","question_id":"ot.1","status":"success","latency_ms":10}`
	gz.Write([]byte(manifest + "\n" + rec + "\n" + rec + "\n"))
	// 关键：flush 一部分数据后不调用 gz.Close() —— 模拟进程崩溃
	gz.Flush()
	f.Close() // 底层文件有数据，但 gzip 流没有终止块

	file, err := Read(p)
	if err != nil {
		t.Fatalf("截断文件应可读回已 flush 的部分: %v", err)
	}
	if !file.Truncated {
		t.Error("Truncated 应为 true")
	}
	if len(file.Records) != 2 {
		t.Errorf("records = %d, want 2", len(file.Records))
	}
	if file.Manifest.PlanDigest != "sha256:aa" {
		t.Errorf("manifest 丢失: %+v", file.Manifest)
	}
	if file.Aggregates != nil {
		t.Error("崩溃文件不应有 aggregates")
	}
}

// 截断文件续跑：EnsureFile 应接受它（digest 一致），已成功的 record 进 resume 集合。
func TestEnsureFileAfterCrash(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "crashed.rawdata.jsonl.gz")

	f, _ := os.Create(p)
	gz := gzip.NewWriter(f)
	gz.Write([]byte(`{"kind":"manifest","format_version":1,"plan_digest":"sha256:aa","collection_plan":{}}` + "\n"))
	gz.Write([]byte(`{"kind":"record","probe_id":"onetoken","question_id":"ot.1","protocol":"p","transport_mode":"non_stream","repeat_index":0,"status":"success","latency_ms":1}` + "\n"))
	gz.Write([]byte(`{"kind":"record","probe_id":"onetoken","question_id":"ot.1","protocol":"p","transport_mode":"non_stream","repeat_index":1,"status":"error","error_kind":"timeout","latency_ms":1}` + "\n"))
	gz.Flush()
	f.Close()

	done, err := EnsureFile(p, Manifest{PlanDigest: "sha256:aa"})
	if err != nil {
		t.Fatalf("崩溃文件应可续跑: %v", err)
	}
	if len(done) != 1 {
		t.Errorf("done = %v, want 只有 success 的 repeat 0", done)
	}
}

// Flush 后立即可读：边采边落盘的语义验证。
func TestFlushDurability(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "out.rawdata.jsonl.gz")

	w, err := Append(p)
	if err != nil {
		t.Fatal(err)
	}
	mb, _ := json.Marshal(Manifest{Kind: "manifest", FormatVersion: 1, PlanDigest: "sha256:aa"})
	w.WriteLine(mb)
	w.WriteRecord(Record{ProbeID: "onetoken", QuestionID: "ot.1", Status: "success", LatencyMs: 1})
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	// 不 Close：模拟进程还活着但文件已有部分数据
	f, err := Read(p)
	if err != nil {
		t.Fatalf("Flush 后应可读: %v", err)
	}
	if len(f.Records) != 1 {
		t.Errorf("records = %d, want 1", len(f.Records))
	}
	w.Close()
}

// 连接失败的 record：http_code 必须是 null（不是 0）。
func TestConnectionFailureHTTPCodeNull(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "out.rawdata.jsonl.gz")
	w, _ := Append(p)
	mb, _ := json.Marshal(Manifest{Kind: "manifest", PlanDigest: "sha256:aa"})
	w.WriteLine(mb)
	kind := "connection"
	rec := Record{ProbeID: "onetoken", QuestionID: "ot.1", Status: "error", ErrorKind: &kind}
	// HTTPCode 留 nil（transport.FillTransport 对未收到响应保持 nil）
	w.WriteRecord(rec)
	w.Close()

	f, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	if f.Records[0].HTTPCode != nil {
		t.Errorf("http_code = %v, want null", *f.Records[0].HTTPCode)
	}
	// 直接断言落盘文本：解压后 record 行含 "http_code":null
	gzf, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer gzf.Close()
	gzr, err := gzip.NewReader(gzf)
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(gzr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), `"http_code":null`) {
		t.Errorf("落盘文本应为 \"http_code\":null，实际内容: %s", content)
	}
}
