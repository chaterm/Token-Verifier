package rawdata

import (
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeRaw 把若干行 JSON 对象写成 gzipped NDJSON 测试文件。
func writeRaw(t *testing.T, lines ...any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.rawdata.jsonl.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := gzip.NewWriter(f)
	enc := json.NewEncoder(w)
	for _, l := range lines {
		if err := enc.Encode(l); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadRoundTrip(t *testing.T) {
	manifest := map[string]any{
		"kind":           "manifest",
		"format_version": 1,
		"tool_version":   "0.1.0",
		"plan_digest":    "sha256:abc",
		"raw_level":      "digest",
		"collection_plan": map[string]any{
			"suite_version": "v1",
			"probes": map[string]any{
				"onetoken": map[string]any{
					"probe_version": "1",
					"repeats":       20,
					"cell_key":      []string{"question_id", "context_bucket"},
				},
			},
		},
	}
	record := map[string]any{
		"kind":           "record",
		"probe_id":       "onetoken",
		"question_id":    "ot.v1.001",
		"context_bucket": 0,
		"protocol":       "openai-chat",
		"transport_mode": "stream",
		"repeat_index":   0,
		"attempt":        0,
		"status":         "success",
		"latency_ms":     842,
		"ttft_ms":        311,
		"usage":          map[string]any{"prompt": 128, "completion": 3},
		"observation":    map[string]any{"value": "7"},
	}
	aggregates := map[string]any{
		"kind":         "aggregates",
		"record_count": 1,
		"cells": map[string]any{
			"onetoken|ot.v1.001|0": map[string]any{"n": 1, "n_valid": 1},
		},
	}

	path := writeRaw(t, manifest, record, aggregates)
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}

	if got.Manifest.PlanDigest != "sha256:abc" || got.Manifest.FormatVersion != 1 {
		t.Errorf("manifest 解析错误: %+v", got.Manifest)
	}
	if len(got.Records) != 1 {
		t.Fatalf("records = %d, want 1", len(got.Records))
	}
	r := got.Records[0]
	if r.ProbeID != "onetoken" || r.QuestionID != "ot.v1.001" || r.TtftMs == nil || *r.TtftMs != 311 {
		t.Errorf("record 解析错误: %+v", r)
	}
	if r.Usage.Prompt != 128 {
		t.Errorf("usage 解析错误: %+v", r.Usage)
	}
	var obs struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(r.Observation, &obs); err != nil || obs.Value != "7" {
		t.Errorf("observation 解析错误: %v %v", obs, err)
	}
	if got.Aggregates == nil || got.Aggregates.RecordCount != 1 {
		t.Errorf("aggregates 解析错误: %+v", got.Aggregates)
	}
	if cell := got.Aggregates.Cells["onetoken|ot.v1.001|0"]; cell.N != 1 || cell.NValid != 1 {
		t.Errorf("cell 聚合解析错误: %+v", cell)
	}

	// collection_plan 泛型解析
	cp, err := got.Manifest.CollectionPlanParsed()
	if err != nil {
		t.Fatal(err)
	}
	if cp.SuiteVersion != "v1" || cp.Probes["onetoken"].Repeats != 20 {
		t.Errorf("collection_plan 解析错误: %+v", cp)
	}
	if len(cp.Probes["onetoken"].CellKey) != 2 {
		t.Errorf("cell_key 解析错误: %v", cp.Probes["onetoken"].CellKey)
	}
}

func TestReadMissingAggregates(t *testing.T) {
	// 缺 aggregates 行 = 采集未完成，仍可读取，Aggregates 为 nil
	manifest := map[string]any{"kind": "manifest", "format_version": 1, "plan_digest": "sha256:x"}
	record := map[string]any{"kind": "record", "probe_id": "onetoken", "status": "success"}
	path := writeRaw(t, manifest, record)

	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Aggregates != nil {
		t.Error("Aggregates 应为 nil")
	}
	if len(got.Records) != 1 {
		t.Errorf("records = %d, want 1", len(got.Records))
	}
}

func TestReadErrors(t *testing.T) {
	// 文件不存在
	if _, err := Read(filepath.Join(t.TempDir(), "nope.gz")); err == nil {
		t.Error("不存在的文件应报错")
	}
	// 缺 manifest
	path := writeRaw(t, map[string]any{"kind": "record"})
	if _, err := Read(path); err == nil {
		t.Error("缺 manifest 应报错")
	}
	// manifest 不在第 1 行
	path = writeRaw(t,
		map[string]any{"kind": "record"},
		map[string]any{"kind": "manifest"},
	)
	if _, err := Read(path); err == nil {
		t.Error("manifest 不在首行应报错")
	}
	// 非法 JSON 行：直接写坏文件
	bad := filepath.Join(t.TempDir(), "bad.gz")
	f, _ := os.Create(bad)
	w := gzip.NewWriter(f)
	w.Write([]byte("not json\n"))
	w.Close()
	f.Close()
	if _, err := Read(bad); err == nil {
		t.Error("非法 JSON 行应报错")
	}
}

func TestReadNullTransportFields(t *testing.T) {
	// 非流式 record：ttft_ms/tps 为 null，必须解析为 nil 指针而非 0
	manifest := map[string]any{"kind": "manifest", "format_version": 1}
	record := map[string]any{
		"kind": "record", "status": "success",
		"ttft_ms": nil, "tps": nil, "http_code": 200,
	}
	path := writeRaw(t, manifest, record)
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	r := got.Records[0]
	if r.TtftMs != nil || r.Tps != nil {
		t.Errorf("null 传输字段应为 nil 指针: ttft=%v tps=%v", r.TtftMs, r.Tps)
	}
	if r.HTTPCode == nil || *r.HTTPCode != 200 {
		t.Errorf("http_code 解析错误: %v", r.HTTPCode)
	}
}

// Read 的错误必须自带文件名与中文措辞，不透 OS 原文。
// 此前 CLI 又要自己拼路径，导致路径出现两次：
// 「ERROR /tmp/a.gz: 打开 rawData 失败: open /tmp/a.gz: The system cannot ...」
func TestReadErrorMessagesIncludePath(t *testing.T) {
	dir := t.TempDir()

	// 文件不存在
	missing := filepath.Join(dir, "nosuch.gz")
	_, err := Read(missing)
	if err == nil {
		t.Fatal("文件不存在应报错")
	}
	if !strings.Contains(err.Error(), "nosuch.gz") {
		t.Errorf("错误应含文件名: %v", err)
	}
	for _, leaked := range []string{"The system cannot find", "no such file", "open "} {
		if strings.Contains(err.Error(), leaked) {
			t.Errorf("不应泄露 OS 原文 %q: %v", leaked, err)
		}
	}

	// 不是 gzip：含文件名
	plain := filepath.Join(dir, "plain.gz")
	if err := os.WriteFile(plain, []byte("not gzip at all"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Read(plain)
	if err == nil {
		t.Fatal("非 gzip 应报错")
	}
	if !strings.Contains(err.Error(), "plain.gz") {
		t.Errorf("gzip 错误应含文件名: %v", err)
	}
}
