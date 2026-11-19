// Package tv 为嵌入式上层平台（如 TV-Watch）提供统一的纯 Go SDK 接口。
//
// 所有采集、协议适配、统计检验、可比性闸门与证据读写原语均直接委托给内部实现，
// 保证上层平台与 tv CLI 的数据格式、plan_digest 与统计口径 100% 同源，绝不双写。
package tv

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/chaterm/token-verifier/internal/adapter"
	"github.com/chaterm/token-verifier/internal/collect"
	"github.com/chaterm/token-verifier/internal/compare"
	"github.com/chaterm/token-verifier/internal/config"
	"github.com/chaterm/token-verifier/internal/plan"
	"github.com/chaterm/token-verifier/internal/probe"
	"github.com/chaterm/token-verifier/internal/rawdata"
	"github.com/chaterm/token-verifier/internal/stats"
	"github.com/chaterm/token-verifier/internal/suite"
	"github.com/chaterm/token-verifier/internal/transport"
)

// —— 核心配置与题库类型导出 ——

type (
	ConfigFile         = config.File
	TargetConfig       = config.Target
	ProtocolConfig     = config.ProtocolConfig
	Capabilities       = config.Capabilities
	TransportMix       = config.TransportMix
	ThinkingBlock      = config.ThinkingBlock
	ThinkingMode       = config.ThinkingMode
	RequestConfig      = config.RequestConfig
	SuiteRef           = config.SuiteRef
	ProbeConfig        = config.ProbeConfig
	RuntimeConfig      = config.Runtime
	PaddingConfig      = config.Padding
	SuiteItemInfo      = config.SuiteItemInfo
	ProbeThresholds    = config.ProbeThresholds
	ResolvedThreshold  = config.ResolvedThreshold
	MissingThresholdErr = config.MissingThresholdError

	SuiteFile      = suite.File
	OutputContract = suite.OutputContract
	NormalizeSpace = suite.NormalizeSpace
	ProbeSet       = suite.ProbeSet
	SuiteTool      = suite.Tool
	SuiteItem      = suite.Item
	Normalizer     = suite.Normalizer

	CollectionPlan = plan.CollectionPlan
	ProbePlan      = plan.ProbePlan

	RawDataFile          = rawdata.File
	RawManifest          = rawdata.Manifest
	RawEndpoint          = rawdata.Endpoint
	RawSkip              = rawdata.Skip
	RawUsage             = rawdata.Usage
	RawRecord            = rawdata.Record
	RawCellAggregate     = rawdata.CellAggregate
	RawTransportMetrics  = rawdata.TransportMetrics
	RawAggregates        = rawdata.Aggregates
	RawTransport         = rawdata.Transport
	RawCollectionPlan    = rawdata.CollectionPlan
	RawProbePlan         = rawdata.ProbePlan

	Probe             = probe.Probe
	ProbeMeta         = probe.ProbeMeta
	StatKind          = probe.StatKind
	CellPair          = probe.CellPair
	Observation       = probe.Observation
	Verdict           = probe.Verdict
	BucketVerdict     = probe.BucketVerdict
	CellDetail        = probe.CellDetail

	CompareResult     = compare.Result
	CompareOptions    = compare.Options
	CompareScope      = compare.Scope
	ScopeExclusion    = compare.ScopeExclusion
	GateReconciled    = compare.Reconciled
	GateError         = compare.GateError
	Percentiles       = compare.Percentiles
	TransportHists    = compare.TransportHists
	CompareTransportMetrics = compare.TransportMetrics

	CollectOptions    = collect.Options
	CollectResult     = collect.Result
	CollectErrSample  = collect.ErrorSample

	ConcurrencyEvent    = transport.ConcurrencyEvent
	ConcurrencySnapshot = transport.ConcurrencySnapshot

	Dist = stats.Dist
	Hist = stats.Hist
)

const (
	StatDistance = probe.StatDistance
	StatPValue   = probe.StatPValue
)

var (
	DefaultMinN    = config.DefaultMinN
	ReservedFields = config.ReservedFields
)

// AllProbeIDs 返回当前引擎支持的全部标准探针 ID（固定排序）。
func AllProbeIDs() []string {
	return []string{"onetoken", "tokenizer", "needle", "think-effort", "toolcall"}
}

// GetProbe 按 ID 查询探针实现。
func GetProbe(id string) (Probe, bool) {
	return probe.Get(id)
}

// —— 题库与计划构建 ——

// LoadSuite 从磁盘路径读取并校验题库。
func LoadSuite(path, wantSHA256 string) (*SuiteFile, error) {
	return suite.Load(path, wantSHA256)
}

// ParseSuite 直接从内存 YAML 字节解析并校验题库。
func ParseSuite(data []byte, wantSHA256 string) (*SuiteFile, error) {
	return suite.Parse(data, wantSHA256)
}

// SuiteItemsMap 提取配置校验所需的探针题目元信息映射。
func SuiteItemsMap(st *SuiteFile) map[string][]SuiteItemInfo {
	out := make(map[string][]SuiteItemInfo, len(st.Probes))
	for pid, ps := range st.Probes {
		items := make([]SuiteItemInfo, 0, len(ps.Items))
		for _, it := range ps.Items {
			items = append(items, SuiteItemInfo{
				ID:             it.ID,
				ThinkingEffort: ps.ThinkingEffortOf(it),
			})
		}
		out[pid] = items
	}
	return out
}

// ValidateInMemory 校验内存构造的采集配置与题库（返回全部非阻断警告切片）。
func ValidateInMemory(cfg *ConfigFile, st *SuiteFile, hasInlineAPIKey bool) ([]string, error) {
	cfg.ApplyDefaults()
	var warnings []string
	err := cfg.ValidateInMemory(SuiteItemsMap(st), hasInlineAPIKey, func(w string) {
		warnings = append(warnings, w)
	})
	return warnings, err
}

// PlanSummary 计划摘要（含 plan_digest、逐探针 digest 与干跑请求/token 估算）。
type PlanSummary struct {
	Plan            *CollectionPlan   `json:"plan"`
	CanonicalJSON   []byte            `json:"canonical_json"`
	PlanDigest      string            `json:"plan_digest"`
	ProbeDigests    map[string]string `json:"probe_digests"`
	TotalRequests   int               `json:"total_requests"`
	EstimatedTokens int               `json:"estimated_tokens"`
	ProbeRequests   map[string]int    `json:"probe_requests"`
}

// BuildPlanSummary 构建采集计划、计算确定性 plan_digest 并估算请求预算（不联网不落盘）。
func BuildPlanSummary(cfg *ConfigFile, st *SuiteFile) (*PlanSummary, error) {
	cp := *cfg
	cp.ApplyDefaults()
	if cp.Version == 0 {
		cp.Version = 1
	}
	// 若仅计算计划摘要而尚未绑定真实端点，填充最小占位以跑通 dry-run 展开
	if cp.Target.BaseURL == "" {
		cp.Target.BaseURL = "https://placeholder.invalid"
	}
	if cp.Target.Model == "" {
		cp.Target.Model = "placeholder"
	}
	if len(cp.Target.Protocols) == 0 {
		cp.Target.Protocols = []ProtocolConfig{{
			ID:           "openai-chat",
			Weight:       1,
			Capabilities: Capabilities{Stream: true, Tools: true, Thinking: true, Present: true},
			Transport:    TransportMix{Stream: 0.5, NonStream: 0.5},
			Thinking: ThinkingBlock{
				Disabled: ThinkingMode{BodyOverrides: map[string]any{}},
				Enabled: ThinkingMode{
					EffortMap:     map[string]any{"low": "low", "medium": "medium", "high": "high"},
					BodyOverrides: map[string]any{"reasoning_effort": "${thinking_effort}"},
				},
			},
		}}
	}

	built, err := plan.Build(&cp, st)
	if err != nil {
		return nil, err
	}
	digest, canonical, err := built.Digest()
	if err != nil {
		return nil, err
	}
	probeDigests := make(map[string]string, len(built.Probes))
	for id := range built.Probes {
		if d, err := built.ProbeDigest(id); err == nil {
			probeDigests[id] = d
		}
	}

	dryRes, err := collect.Run(context.Background(), &cp, st, collect.Options{
		DryRun:    true,
		DryRunOut: io.Discard,
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		return nil, err
	}
	return &PlanSummary{
		Plan:            built,
		CanonicalJSON:   canonical,
		PlanDigest:      digest,
		ProbeDigests:    probeDigests,
		TotalRequests:   dryRes.TotalRequests,
		EstimatedTokens: dryRes.EstimatedTokens,
		ProbeRequests:   dryRes.ProbeRequests,
	}, nil
}

// —— 在线采集与端点探活 ——

// Collect 执行一次在线采集并将证据写入 opts.OutputPath（支持断点续跑、内存 APIKey 与 AIMD 并发回调）。
func Collect(ctx context.Context, cfg *ConfigFile, st *SuiteFile, opts CollectOptions) (*CollectResult, error) {
	cfg.ApplyDefaults()
	return collect.Run(ctx, cfg, st, opts)
}

// EndpointProbeItem 单协议单传输模式的探活结果。
type EndpointProbeItem struct {
	Protocol         string `json:"protocol"`
	TransportMode    string `json:"transport_mode"`
	Success          bool   `json:"success"`
	HTTPCode         int    `json:"http_code"`
	ErrorKind        string `json:"error_kind,omitempty"`
	ErrorMessage     string `json:"error_message,omitempty"`
	LatencyMs        int    `json:"latency_ms"`
	TtftMs           *int   `json:"ttft_ms,omitempty"`
	Tps              *float64 `json:"tps,omitempty"`
	ContentPreview   string `json:"content_preview,omitempty"`
	ReasoningPreview string `json:"reasoning_preview,omitempty"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	ReasoningTokens  int    `json:"reasoning_tokens"`
	CachedTokens     int    `json:"cached_tokens"`
}

// ProbeEndpoint 复用 TV 原生协议适配器与传输层对 Target 配置执行最小连通性与能力探活。
func ProbeEndpoint(ctx context.Context, target TargetConfig, apiKey string, timeoutSec int) ([]EndpointProbeItem, error) {
	if timeoutSec <= 0 {
		timeoutSec = 30
	}
	if target.Request.TemperatureField == "" {
		target.Request.TemperatureField = "temperature"
	}
	client := transport.New(transport.Options{
		MaxConcurrency: 2,
		TimeoutSec:     timeoutSec,
		MaxAttempts:    1,
	})
	maxTok := 32
	temp := 0.0

	var results []EndpointProbeItem
	for _, proto := range target.Protocols {
		if proto.Weight <= 0 {
			continue
		}
		ad, ok := adapter.Get(proto.ID)
		if !ok {
			results = append(results, EndpointProbeItem{
				Protocol:     proto.ID,
				ErrorKind:    "protocol",
				ErrorMessage: fmt.Sprintf("未知协议适配器: %s", proto.ID),
			})
			continue
		}
		mode := "non_stream"
		if proto.Transport.Stream > 0 && proto.Capabilities.Stream {
			mode = "stream"
		}
		mergedOverrides := adapter.DeepMerge(target.Request.BodyOverrides, proto.Thinking.Disabled.BodyOverrides)
		res := client.Do(ctx, transport.Task{
			Adapter: ad,
			BaseURL: target.BaseURL,
			APIKey:  apiKey,
			Logical: adapter.LogicalRequest{
				Model:            target.Model,
				System:           "Reply concisely.",
				Prompt:           "Echo exactly: Hello TV-Watch",
				MaxTokens:        &maxTok,
				Temperature:      &temp,
				Stream:           mode == "stream",
				Path:             proto.Path,
				TemperatureField: target.Request.TemperatureField,
			},
			Thinking:      adapter.ThinkingBlock{BodyOverrides: mergedOverrides},
			TransportMode: mode,
			RedactSecret:  apiKey,
		})
		item := EndpointProbeItem{
			Protocol:         proto.ID,
			TransportMode:    mode,
			Success:          res.ErrorKind == "",
			HTTPCode:         res.HTTPCode,
			ErrorKind:        res.ErrorKind,
			ErrorMessage:     res.ErrorMsg,
			LatencyMs:        res.LatencyMs,
			ContentPreview:   truncateRunes(res.Response.Content, 160),
			ReasoningPreview: truncateRunes(res.Response.ReasoningContent, 160),
			PromptTokens:     res.Response.Usage.Prompt,
			CompletionTokens: res.Response.Usage.Completion,
			ReasoningTokens:  res.Response.Usage.Reasoning,
			CachedTokens:     res.Response.Usage.Cached,
		}
		if mode == "stream" && res.TtftMs >= 0 {
			v := res.TtftMs
			item.TtftMs = &v
		}
		if mode == "stream" && res.Tps > 0 {
			v := res.Tps
			item.Tps = &v
		}
		results = append(results, item)
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("未配置任何 weight > 0 的协议")
	}
	return results, nil
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// —— 离线证据读取、可比性闸门、对比与统计原语 ——

// ReadRawData 从文件路径读取 .rawdata.jsonl.gz。
func ReadRawData(path string) (*RawDataFile, error) {
	return rawdata.Read(path)
}

// ReadRawDataStream 从 gzip 字节流读取 rawData。
func ReadRawDataStream(r io.Reader) (*RawDataFile, error) {
	return rawdata.ReadStream(r)
}

// Gate 执行可比性闸门校验与子集调和。
func Gate(a, b *RawDataFile, allowSubset bool) (*GateReconciled, error) {
	return compare.Gate(a, b, allowSubset)
}

// PairCells 将两份 rawData 在指定探针下配对为确定性排序的 CellPair 切片。
func PairCells(a, b *RawDataFile, probeID string, cellKey []string, subset bool) []CellPair {
	return compare.PairCells(a, b, probeID, cellKey, subset)
}

// Compare 离线比较两份 rawData。
func Compare(a, b *RawDataFile, thresholds map[string]float64, cfg *ConfigFile, opts CompareOptions) (*CompareResult, error) {
	return compare.RunWith(a, b, thresholds, cfg, opts)
}

// 统计原语转出，保证上层 ROC/标定与底层探针算法绝对一致。
var (
	JSD             = stats.JSD
	FromSamples     = stats.FromSamples
	Chi2Homogeneity = stats.Chi2Homogeneity
	MannWhitneyU    = stats.MannWhitneyU
	FisherCombine   = stats.FisherCombine
	JointHist       = stats.JointHist
)
