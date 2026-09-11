// Package rawdata 负责 rawData 文件（gzip 压缩的 NDJSON）的类型定义与流式读取。
// 文件格式见 docs/SPEC-RAWDATA.md。
package rawdata

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// Manifest 文件第 1 行：采集计划与元信息。
type Manifest struct {
	Kind           string            `json:"kind"`
	FormatVersion  int               `json:"format_version"`
	ToolVersion    string            `json:"tool_version"`
	CreatedAt      string            `json:"created_at"`
	CollectionPlan json.RawMessage   `json:"collection_plan"` // 保留原始 JSON，供 digest 闸门与逐字段 diff
	PlanDigest     string            `json:"plan_digest"`
	ProbeDigests   map[string]string `json:"probe_digests"`
	Endpoint       Endpoint          `json:"endpoint"`
	Capabilities   json.RawMessage   `json:"capabilities"`
	Skipped        []Skip            `json:"skipped"`
	Sampling       json.RawMessage   `json:"sampling"`
	RawLevel       string            `json:"raw_level"`
}

// Endpoint 端点标识信息（不含明文密钥、主机名与完整 URL）。
type Endpoint struct {
	Model             string `json:"model"`
	APIKeyFingerprint string `json:"api_key_fingerprint"`
	// BaseURLFingerprint base_url 主机名的 SHA-256 前 16 位十六进制。
	// 只用于识别「是否换过端点」，不暴露厂商/网关身份（rawData 可公开）。
	BaseURLFingerprint string `json:"base_url_fingerprint"`
}

// Skip 被跳过的 (探针, 协议) 组合及原因。
type Skip struct {
	ProbeID  string `json:"probe_id"`
	Protocol string `json:"protocol"`
	Reason   string `json:"reason"`
	Detail   string `json:"detail"`
}

// Usage 归一后的 token 计数。
type Usage struct {
	Prompt     int `json:"prompt"`
	Completion int `json:"completion"`
	Reasoning  int `json:"reasoning"`
	Cached     int `json:"cached"`
}

// Record 每次请求尝试一条。
type Record struct {
	Kind string `json:"kind"`

	// 定位字段
	ProbeID        string  `json:"probe_id"`
	QuestionID     string  `json:"question_id"`
	ContextBucket  int     `json:"context_bucket"`
	Protocol       string  `json:"protocol"`
	TransportMode  string  `json:"transport_mode"`
	ThinkingEffort *string `json:"thinking_effort"`
	RepeatIndex    int     `json:"repeat_index"`
	Attempt        int     `json:"attempt"`

	// 传输字段
	Status      string   `json:"status"` // success | error
	HTTPCode    *int     `json:"http_code"`
	ErrorKind   *string  `json:"error_kind"`
	ErrorDetail *string  `json:"error_detail"`
	LatencyMs   int      `json:"latency_ms"`
	TtftMs      *int     `json:"ttft_ms"` // 非流式为 null
	Tps         *float64 `json:"tps"`     // 非流式为 null
	Usage       Usage    `json:"usage"`

	// 观测字段：结构由探针定义，保留原始 JSON 由探针自行解析
	Observation json.RawMessage `json:"observation"`

	RawContentSHA256 string          `json:"raw_content_sha256"`
	RawContent       json.RawMessage `json:"raw_content,omitempty"`
}

// CellAggregate 单 cell 的聚合摘要。
type CellAggregate struct {
	N            int            `json:"n"`
	NValid       int            `json:"n_valid"`
	Distribution map[string]int `json:"distribution,omitempty"`
	Values       []int          `json:"values,omitempty"`
}

// TransportMetrics 一组传输指标。
type TransportMetrics struct {
	Availability float64            `json:"availability"`
	ErrorRate    float64            `json:"error_rate"`
	TimeoutRate  float64            `json:"timeout_rate"`
	LatencyMs    map[string]float64 `json:"latency_ms"` // p50/p90/p99
	TtftMs       map[string]float64 `json:"ttft_ms"`    // p50/p90/p99
	Tps          map[string]float64 `json:"tps"`        // p50/p90
}

// Aggregates 文件最后一行：逐 cell 摘要与分层传输指标。
type Aggregates struct {
	Kind        string                   `json:"kind"`
	RecordCount int                      `json:"record_count"`
	Cells       map[string]CellAggregate `json:"cells"`
	Transport   Transport                `json:"transport"`
}

// Transport 分层传输指标。
type Transport struct {
	Overall             TransportMetrics            `json:"overall"`
	ByProtocol          map[string]TransportMetrics `json:"by_protocol"`
	ByTransportMode     map[string]TransportMetrics `json:"by_transport_mode"`
	ByProtocolTransport map[string]TransportMetrics `json:"by_protocol_transport"`
}

// File 读入内存的完整 rawData：manifest + 全部 record + aggregates（可能缺失）。
// Compare 阶段需要按 cell 分组配对，record 必须驻留内存；
// 读取本身仍是逐行流式的，不会一次反序列化整个文件。
type File struct {
	Manifest   Manifest
	Records    []Record
	Aggregates *Aggregates // nil 表示采集未完成（缺 aggregates 行）
	Truncated  bool        // 尾部 gzip 成员不完整（上次采集中断）；已读到的部分有效
}

// Read 打开并读取一个 rawData 文件。
// 文件不存在、gzip 损坏、JSON 行解析失败、缺 manifest 都返回错误。
func Read(path string) (*File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("打开 rawData 失败: %w", err)
	}
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("gzip 解压失败: %w", err)
	}
	defer func() { _ = gz.Close() }()

	return parse(gz)
}

// parse 逐行解析 NDJSON。第 1 行必须是 manifest，其余按 kind 分派。
// bufio.Scanner 只在见到换行符后才返回一行，所以崩溃残留的半行永远不会
// 被提交 —— 尾部不完整的行连同读取错误一起被丢弃，已读部分保留并标记
// Truncated（多成员 gzip 由 gzip.Reader 天然拼接；尾部成员不完整时读到
// ErrUnexpectedEOF，续跑语义依赖「已读部分仍然有效」）。
func parse(r io.Reader) (*File, error) {
	out := &File{}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // record 行可能较长
	lineNo := 0

	for scanner.Scan() {
		line := scanner.Bytes()
		lineNo++
		if len(line) == 0 {
			continue
		}
		if err := out.commitLine(line, lineNo); err != nil {
			return nil, err
		}
	}

	scanErr := scanner.Err()
	if scanErr != nil {
		if !isTruncation(scanErr) {
			return nil, fmt.Errorf("读取中断: %w", scanErr)
		}
		// 尾部 gzip 成员不完整：保留已解析部分
		out.Truncated = true
	}
	if out.Manifest.Kind != "manifest" {
		return nil, fmt.Errorf("文件缺少 manifest 行")
	}
	return out, nil
}

// commitLine 提交一行已确认完整（以换行结束）的 NDJSON。
func (out *File) commitLine(line []byte, lineNo int) error {
	var kind struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(line, &kind); err != nil {
		return fmt.Errorf("第 %d 行不是合法 JSON: %w", lineNo, err)
	}
	switch kind.Kind {
	case "manifest":
		if lineNo != 1 {
			return fmt.Errorf("第 %d 行出现第二个 manifest", lineNo)
		}
		if err := json.Unmarshal(line, &out.Manifest); err != nil {
			return fmt.Errorf("第 %d 行 manifest 解析失败: %w", lineNo, err)
		}
	case "record":
		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil {
			return fmt.Errorf("第 %d 行 record 解析失败: %w", lineNo, err)
		}
		out.Records = append(out.Records, rec)
	case "aggregates":
		var agg Aggregates
		if err := json.Unmarshal(line, &agg); err != nil {
			return fmt.Errorf("第 %d 行 aggregates 解析失败: %w", lineNo, err)
		}
		out.Aggregates = &agg
	default:
		// 未知 kind 忽略：格式演进时旧读取器应容忍新行（SPEC-RAWDATA §8）
	}
	return nil
}

// isTruncation 判断 gzip 读取错误是否为「尾部成员不完整」。
func isTruncation(err error) bool {
	return errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, gzip.ErrChecksum) ||
		errors.Is(err, io.EOF)
}

// CollectionPlan 把 manifest 里的 collection_plan 解析为泛型结构，
// 供 digest 闸门做逐字段 diff 与读取各探针的 cell_key。
func (m *Manifest) CollectionPlanParsed() (*CollectionPlan, error) {
	var cp CollectionPlan
	if err := json.Unmarshal(m.CollectionPlan, &cp); err != nil {
		return nil, fmt.Errorf("collection_plan 解析失败: %w", err)
	}
	return &cp, nil
}

// CollectionPlan 采集计划（compare 侧只关心 probes 的元信息）。
type CollectionPlan struct {
	SuiteVersion       string               `json:"suite_version"`
	QuestionSetDigest  string               `json:"question_set_digest"`
	PaddingAlgoVersion string               `json:"padding_algo_version"`
	PaddingSeed        int64                `json:"padding_seed"`
	Probes             map[string]ProbePlan `json:"probes"`
}

// ProbePlan 单探针的采集计划。
type ProbePlan struct {
	ProbeVersion      string   `json:"probe_version"`
	Repeats           int      `json:"repeats"`
	Temperature       *float64 `json:"temperature"`
	MaxTokens         *int     `json:"max_tokens"`
	ContextBuckets    []int    `json:"context_buckets"`
	ThinkingEffort    *string  `json:"thinking_effort"`
	NormalizeRule     string   `json:"normalize_rule"`
	ObservationSchema string   `json:"observation_schema"`
	CellKey           []string `json:"cell_key"`
	QuestionIDs       []string `json:"question_ids"`
	// MinN 生效的单侧样本下限。旧文件无该字段 → 0，比较侧回退探针默认。
	MinN int `json:"min_n"`
}
