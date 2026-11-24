package tv

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/chaterm/token-verifier/internal/adapter"
	"github.com/chaterm/token-verifier/internal/config"
	"github.com/chaterm/token-verifier/internal/transport"
)

const (
	maxParamProbeTotalCases        = 24
	maxParamProbeCustomCases       = 8
	maxParamProbeEffortCandidates  = 8
	maxParamProbeExtraTemperatures = 6
	maxParamProbeTimeoutSec        = 600
	paramProbeTempPrompt           = "Reply with ok."
	paramProbeThinkingPrompt       = "What is 17 * 23? Answer with the final number only."
)

// ParamProbeCustomCase 自定义参数兼容探测用例（用于验证特定温度或 body_overrides 假设）。
type ParamProbeCustomCase struct {
	Label         string         `json:"label"`
	Temperature   *float64       `json:"temperature,omitempty"`
	BodyOverrides map[string]any `json:"body_overrides,omitempty"`
	Stream        *bool          `json:"stream,omitempty"`
	MaxTokens     *int           `json:"max_tokens,omitempty"`
	Prompt        string         `json:"prompt,omitempty"`
}

// ParamProbeOptions 端点参数兼容性轻量探测选项（零值即开启全部默认探测项）。
type ParamProbeOptions struct {
	ProtocolID             string                 `json:"protocol_id,omitempty"`
	TimeoutSec             int                    `json:"timeout_sec,omitempty"`
	IncludeTemperature     *bool                  `json:"include_temperature,omitempty"`
	ExtraTemperatures      []float64              `json:"extra_temperatures,omitempty"`
	IncludeConfigured      *bool                  `json:"include_configured,omitempty"`
	IncludeThinkingDisable *bool                  `json:"include_thinking_disable,omitempty"`
	EffortCandidates       *[]string              `json:"effort_candidates,omitempty"`
	FixedTemperature       *float64               `json:"fixed_temperature,omitempty"`
	CustomCases            []ParamProbeCustomCase `json:"custom_cases,omitempty"`
}

// ParamProbeCaseResult 单条参数兼容探测用例的实测结果。
type ParamProbeCaseResult struct {
	Category         string         `json:"category"`
	Label            string         `json:"label"`
	Protocol         string         `json:"protocol"`
	TransportMode    string         `json:"transport_mode"`
	SentTemperature  *float64       `json:"sent_temperature,omitempty"`
	SentOverrides    map[string]any `json:"sent_overrides,omitempty"`
	Success          bool           `json:"success"`
	HTTPCode         int            `json:"http_code"`
	ErrorKind        string         `json:"error_kind,omitempty"`
	ErrorMessage     string         `json:"error_message,omitempty"`
	LatencyMs        int            `json:"latency_ms"`
	ContentPreview   string         `json:"content_preview,omitempty"`
	ReasoningPreview string         `json:"reasoning_preview,omitempty"`
	PromptTokens     int            `json:"prompt_tokens"`
	CompletionTokens int            `json:"completion_tokens"`
	ReasoningTokens  int            `json:"reasoning_tokens"`
	ReasoningChars   int            `json:"reasoning_chars"`
}

// ParamProbeRejection 单项参数被网关拒绝的摘要。
type ParamProbeRejection struct {
	Value    string `json:"value"`
	HTTPCode int    `json:"http_code"`
	Error    string `json:"error"`
}

// ParamProbeEffortResult 被网关接受的单个思考档位实测观测。
type ParamProbeEffortResult struct {
	Value           string `json:"value"`
	ReasoningTokens int    `json:"reasoning_tokens"`
	ReasoningChars  int    `json:"reasoning_chars"`
	LatencyMs       int    `json:"latency_ms"`
}

// ParamProbeConfiguredEffort 当前端点已配置 effort_map 档位的实测结果。
type ParamProbeConfiguredEffort struct {
	MappedValue     any    `json:"mapped_value,omitempty"`
	OK              bool   `json:"ok"`
	HTTPCode        int    `json:"http_code"`
	ReasoningTokens int    `json:"reasoning_tokens"`
	ReasoningChars  int    `json:"reasoning_chars"`
	Error           string `json:"error,omitempty"`
}

// ParamProbeTemperatureSummary 温度兼容性汇总。
type ParamProbeTemperatureSummary struct {
	OmittedOK *bool                 `json:"omitted_ok,omitempty"`
	Accepted  []float64             `json:"accepted"`
	Rejected  []ParamProbeRejection `json:"rejected"`
}

// ParamProbeThinkingSummary 思考开关与档位兼容性汇总。
type ParamProbeThinkingSummary struct {
	EffectiveTemperature       *float64                              `json:"effective_temperature,omitempty"`
	DefaultProducesReasoning   *bool                                 `json:"default_produces_reasoning,omitempty"`
	DisableAccepted            []string                              `json:"disable_accepted"`
	DisableEffective           []string                              `json:"disable_effective"`
	DisableRejected            []ParamProbeRejection                 `json:"disable_rejected"`
	EffortAccepted             []ParamProbeEffortResult              `json:"effort_accepted"`
	EffortRejected             []ParamProbeRejection                 `json:"effort_rejected"`
	ConfiguredDisabledOK       *bool                                 `json:"configured_disabled_ok,omitempty"`
	ConfiguredDisabledReasonTk int                                   `json:"configured_disabled_reasoning_tokens,omitempty"`
	ConfiguredEfforts          map[string]ParamProbeConfiguredEffort `json:"configured_efforts,omitempty"`
}

// ParamProbeResult 端点参数兼容性实测总报告。
type ParamProbeResult struct {
	Protocol              string                       `json:"protocol"`
	TransportMode         string                       `json:"transport_mode"`
	Model                 string                       `json:"model"`
	TemperatureSummary    ParamProbeTemperatureSummary `json:"temperature_summary"`
	ThinkingSummary       ParamProbeThinkingSummary    `json:"thinking_summary"`
	Cases                 []ParamProbeCaseResult       `json:"cases"`
	TotalRequests         int                          `json:"total_requests"`
	TotalPromptTokens     int                          `json:"total_prompt_tokens"`
	TotalCompletionTokens int                          `json:"total_completion_tokens"`
	TotalReasoningTokens  int                          `json:"total_reasoning_tokens"`
	DurationMs            int                          `json:"duration_ms"`
	Truncated             bool                         `json:"truncated,omitempty"`
	ProbedAt              time.Time                    `json:"probed_at"`
}

type paramProbePlanCase struct {
	category    string
	label       string
	valueTag    string
	mappedValue any
	prompt      string
	maxTokens   int
	temperature *float64
	overrides   map[string]any
	stream      bool
}

// ProbeEndpointParams 对端点执行轻量参数兼容性探测（温度省略/0/1/自定义、当前思考配置、
// 思考关闭方式是否真正抑制 reasoning、各 effort 档位接受度与思考量），不修改调用方 target。
func ProbeEndpointParams(
	ctx context.Context,
	target TargetConfig,
	apiKey string,
	opts ParamProbeOptions,
) (*ParamProbeResult, error) {
	start := time.Now()
	proto, ad, mode, err := selectProbeProtocol(target, opts.ProtocolID)
	if err != nil {
		return nil, err
	}

	// 单请求超时跟随被测端点：调用方（TV-Watch）缺省会传端点自身 runtime.timeout_sec
	// （慢端点可达数百秒），上限仅防御离谱值，避免轻量探测被慢模型误判 timeout。
	timeoutSec := opts.TimeoutSec
	if timeoutSec <= 0 {
		timeoutSec = 30
	}
	if timeoutSec > maxParamProbeTimeoutSec {
		timeoutSec = maxParamProbeTimeoutSec
	}

	tempField := target.Request.TemperatureField
	if tempField == "" {
		tempField = "temperature"
	}

	// 剥离 request.body_overrides 中可能残留的 temperature_field，保证温度探测纯净隔离。
	baseOV := stripDotPath(cloneAnyMap(target.Request.BodyOverrides), tempField)

	includeTemp := opts.IncludeTemperature == nil || *opts.IncludeTemperature
	includeConfigured := opts.IncludeConfigured == nil || *opts.IncludeConfigured
	includeDisable := opts.IncludeThinkingDisable == nil || *opts.IncludeThinkingDisable

	client := transport.New(transport.Options{
		MaxConcurrency: 3,
		TimeoutSec:     timeoutSec,
		MaxAttempts:    1,
	})

	var allCases []ParamProbeCaseResult
	truncated := false
	budget := maxParamProbeTotalCases

	runBatch := func(planned []paramProbePlanCase) []ParamProbeCaseResult {
		if len(planned) == 0 || budget <= 0 {
			return nil
		}
		if len(planned) > budget {
			planned = planned[:budget]
			truncated = true
		}
		budget -= len(planned)

		tasks := make([]transport.Task, len(planned))
		for i, pc := range planned {
			mt := pc.maxTokens
			if mt <= 0 {
				mt = 64
			}
			// Anthropic 要求 max_tokens > thinking.budget_tokens，自动抬升防止误报 400。
			if proto.ID == "anthropic-messages" {
				if bt := extractAnthropicBudgetTokens(pc.overrides); bt > 0 && mt <= bt {
					mt = bt + 128
				}
			}
			tMode := "non_stream"
			if pc.stream {
				tMode = "stream"
			}
			tasks[i] = transport.Task{
				Adapter: ad,
				BaseURL: target.BaseURL,
				APIKey:  apiKey,
				Logical: adapter.LogicalRequest{
					Model:            target.Model,
					System:           "Reply concisely.",
					Prompt:           pc.prompt,
					MaxTokens:        &mt,
					Temperature:      pc.temperature,
					Stream:           pc.stream,
					Path:             proto.Path,
					TemperatureField: tempField,
				},
				Thinking:      adapter.ThinkingBlock{BodyOverrides: pc.overrides},
				TransportMode: tMode,
				RedactSecret:  apiKey,
			}
		}

		out := make([]ParamProbeCaseResult, len(planned))
		var mu sync.Mutex
		runner := transport.NewRunner(client)
		runner.Run(ctx, tasks, func(idx int, _ int, tr transport.Result) {
			pc := planned[idx]
			tMode := "non_stream"
			if pc.stream {
				tMode = "stream"
			}
			r := ParamProbeCaseResult{
				Category:         pc.category,
				Label:            pc.label,
				Protocol:         proto.ID,
				TransportMode:    tMode,
				SentTemperature:  pc.temperature,
				SentOverrides:    cloneAnyMap(pc.overrides),
				Success:          tr.ErrorKind == "",
				HTTPCode:         tr.HTTPCode,
				ErrorKind:        tr.ErrorKind,
				ErrorMessage:     tr.ErrorMsg,
				LatencyMs:        tr.LatencyMs,
				ContentPreview:   truncateRunes(tr.Response.Content, 160),
				ReasoningPreview: truncateRunes(tr.Response.ReasoningContent, 160),
				PromptTokens:     tr.Response.Usage.Prompt,
				CompletionTokens: tr.Response.Usage.Completion,
				ReasoningTokens:  tr.Response.Usage.Reasoning,
				ReasoningChars:   utf8.RuneCountInString(tr.Response.ReasoningContent),
			}
			mu.Lock()
			out[idx] = r
			mu.Unlock()
		})
		return out
	}

	tempSummary := ParamProbeTemperatureSummary{
		Accepted: []float64{},
		Rejected: []ParamProbeRejection{},
	}

	// 阶段 1：温度兼容性探测（独立隔离，不混入 thinking 参数）
	var tempPlanned []paramProbePlanCase
	if includeTemp {
		tempPlanned = append(tempPlanned, paramProbePlanCase{
			category:  "temperature",
			label:     "temp:omitted",
			valueTag:  "omitted",
			prompt:    paramProbeTempPrompt,
			maxTokens: 32,
			overrides: cloneAnyMap(baseOV),
			stream:    mode == "stream",
		})
		seenTemps := map[string]bool{}
		addTemp := func(v float64) {
			if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 2 {
				return
			}
			key := strconv.FormatFloat(v, 'f', -1, 64)
			if seenTemps[key] {
				return
			}
			seenTemps[key] = true
			tvVal := v
			tempPlanned = append(tempPlanned, paramProbePlanCase{
				category:    "temperature",
				label:       "temp:" + key,
				valueTag:    key,
				prompt:      paramProbeTempPrompt,
				maxTokens:   32,
				temperature: &tvVal,
				overrides:   cloneAnyMap(baseOV),
				stream:      mode == "stream",
			})
		}
		addTemp(0.0)
		addTemp(1.0)
		extra := opts.ExtraTemperatures
		if len(extra) > maxParamProbeExtraTemperatures {
			extra = extra[:maxParamProbeExtraTemperatures]
		}
		for _, v := range extra {
			addTemp(v)
		}
	}

	tempResults := runBatch(tempPlanned)
	allCases = append(allCases, tempResults...)
	for i, r := range tempResults {
		if i >= len(tempPlanned) {
			break
		}
		pc := tempPlanned[i]
		if pc.temperature == nil {
			ok := r.Success
			tempSummary.OmittedOK = &ok
			if !r.Success {
				tempSummary.Rejected = append(tempSummary.Rejected, ParamProbeRejection{
					Value:    "omitted",
					HTTPCode: r.HTTPCode,
					Error:    r.ErrorMessage,
				})
			}
		} else if r.Success {
			tempSummary.Accepted = append(tempSummary.Accepted, *pc.temperature)
		} else {
			tempSummary.Rejected = append(tempSummary.Rejected, ParamProbeRejection{
				Value:    pc.valueTag,
				HTTPCode: r.HTTPCode,
				Error:    r.ErrorMessage,
			})
		}
	}

	// 决定阶段 2（思考开关/档位）使用的温度：优先用显式 FixedTemperature；
	// 否则若温度省略可用则不传温度；若网关强制要求显式温度，则自动回退到阶段 1 首个被接受的温度。
	var thinkingTemp *float64
	if opts.FixedTemperature != nil {
		v := *opts.FixedTemperature
		thinkingTemp = &v
	} else if tempSummary.OmittedOK != nil && !*tempSummary.OmittedOK && len(tempSummary.Accepted) > 0 {
		v := tempSummary.Accepted[0]
		for _, a := range tempSummary.Accepted {
			if a == 1.0 {
				v = a
				break
			}
		}
		thinkingTemp = &v
	}

	// 阶段 2：当前已配思考块 + 关闭方式发现 + effort 档位发现 + 自定义用例
	var stage2 []paramProbePlanCase

	if includeConfigured {
		disabledOV := adapter.DeepMerge(baseOV, proto.Thinking.Disabled.BodyOverrides)
		stage2 = append(stage2, paramProbePlanCase{
			category:    "thinking_configured",
			label:       "configured:disabled",
			valueTag:    "disabled",
			prompt:      paramProbeThinkingPrompt,
			maxTokens:   512,
			temperature: thinkingTemp,
			overrides:   disabledOV,
			stream:      mode == "stream",
		})
		if len(proto.Thinking.Enabled.EffortMap) > 0 {
			keys := make([]string, 0, len(proto.Thinking.Enabled.EffortMap))
			for k := range proto.Thinking.Enabled.EffortMap {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				mapped := proto.Thinking.Enabled.EffortMap[k]
				expanded, _ := adapter.ExpandPlaceholders(proto.Thinking.Enabled.BodyOverrides, mapped).(map[string]any)
				ov := adapter.DeepMerge(baseOV, expanded)
				stage2 = append(stage2, paramProbePlanCase{
					category:    "thinking_configured",
					label:       "configured:effort:" + k,
					valueTag:    k,
					mappedValue: mapped,
					prompt:      paramProbeThinkingPrompt,
					maxTokens:   512,
					temperature: thinkingTemp,
					overrides:   ov,
					stream:      mode == "stream",
				})
			}
		}
	}

	if includeDisable {
		for _, dc := range defaultDisableCandidates(proto.ID) {
			stage2 = append(stage2, paramProbePlanCase{
				category:    "thinking_disable",
				label:       dc.label,
				valueTag:    dc.tag,
				prompt:      paramProbeThinkingPrompt,
				maxTokens:   512,
				temperature: thinkingTemp,
				overrides:   adapter.DeepMerge(baseOV, dc.overrides),
				stream:      mode == "stream",
			})
		}
	}

	efforts := resolveEffortCandidates(proto.ID, opts.EffortCandidates)
	for _, eff := range efforts {
		ov := effortOverrideForProtocol(proto.ID, eff)
		if ov == nil {
			continue
		}
		stage2 = append(stage2, paramProbePlanCase{
			category:    "thinking_effort",
			label:       "effort:" + eff,
			valueTag:    eff,
			prompt:      paramProbeThinkingPrompt,
			maxTokens:   512,
			temperature: thinkingTemp,
			overrides:   adapter.DeepMerge(baseOV, ov),
			stream:      mode == "stream",
		})
	}

	custom := opts.CustomCases
	if len(custom) > maxParamProbeCustomCases {
		custom = custom[:maxParamProbeCustomCases]
		truncated = true
	}
	for i, cc := range custom {
		if err := checkCustomOverridesReserved(cc.BodyOverrides); err != nil {
			return nil, fmt.Errorf("custom_cases[%d]: %w", i, err)
		}
		lbl := strings.TrimSpace(cc.Label)
		if lbl == "" {
			lbl = fmt.Sprintf("custom:%d", i+1)
		}
		p := strings.TrimSpace(cc.Prompt)
		if p == "" {
			p = paramProbeThinkingPrompt
		}
		mt := 256
		if cc.MaxTokens != nil && *cc.MaxTokens > 0 {
			mt = *cc.MaxTokens
			if mt > 1024 {
				mt = 1024
			}
		}
		stFlag := mode == "stream"
		if cc.Stream != nil {
			stFlag = *cc.Stream
		}
		stage2 = append(stage2, paramProbePlanCase{
			category:    "custom",
			label:       lbl,
			valueTag:    lbl,
			prompt:      p,
			maxTokens:   mt,
			temperature: cc.Temperature,
			overrides:   adapter.DeepMerge(baseOV, cc.BodyOverrides),
			stream:      stFlag,
		})
	}

	stage2Results := runBatch(stage2)
	allCases = append(allCases, stage2Results...)

	thinkSummary := ParamProbeThinkingSummary{
		EffectiveTemperature: thinkingTemp,
		DisableAccepted:      []string{},
		DisableEffective:     []string{},
		DisableRejected:      []ParamProbeRejection{},
		EffortAccepted:       []ParamProbeEffortResult{},
		EffortRejected:       []ParamProbeRejection{},
	}

	for i, r := range stage2Results {
		if i >= len(stage2) {
			break
		}
		pc := stage2[i]
		hasReasoning := r.ReasoningTokens > 0 || r.ReasoningChars > 0
		switch pc.category {
		case "thinking_configured":
			if pc.valueTag == "disabled" {
				ok := r.Success
				thinkSummary.ConfiguredDisabledOK = &ok
				thinkSummary.ConfiguredDisabledReasonTk = r.ReasoningTokens
			} else {
				if thinkSummary.ConfiguredEfforts == nil {
					thinkSummary.ConfiguredEfforts = map[string]ParamProbeConfiguredEffort{}
				}
				thinkSummary.ConfiguredEfforts[pc.valueTag] = ParamProbeConfiguredEffort{
					MappedValue:     pc.mappedValue,
					OK:              r.Success,
					HTTPCode:        r.HTTPCode,
					ReasoningTokens: r.ReasoningTokens,
					ReasoningChars:  r.ReasoningChars,
					Error:           r.ErrorMessage,
				}
			}
		case "thinking_disable":
			if pc.valueTag == "default_omit" {
				if r.Success {
					prod := hasReasoning
					thinkSummary.DefaultProducesReasoning = &prod
				}
				continue
			}
			if r.Success {
				thinkSummary.DisableAccepted = append(thinkSummary.DisableAccepted, pc.valueTag)
				if !hasReasoning {
					thinkSummary.DisableEffective = append(thinkSummary.DisableEffective, pc.valueTag)
				}
			} else {
				thinkSummary.DisableRejected = append(thinkSummary.DisableRejected, ParamProbeRejection{
					Value:    pc.valueTag,
					HTTPCode: r.HTTPCode,
					Error:    r.ErrorMessage,
				})
			}
		case "thinking_effort":
			if r.Success {
				thinkSummary.EffortAccepted = append(thinkSummary.EffortAccepted, ParamProbeEffortResult{
					Value:           pc.valueTag,
					ReasoningTokens: r.ReasoningTokens,
					ReasoningChars:  r.ReasoningChars,
					LatencyMs:       r.LatencyMs,
				})
			} else {
				thinkSummary.EffortRejected = append(thinkSummary.EffortRejected, ParamProbeRejection{
					Value:    pc.valueTag,
					HTTPCode: r.HTTPCode,
					Error:    r.ErrorMessage,
				})
			}
		}
	}

	var totPrompt, totComp, totReason int
	for _, c := range allCases {
		totPrompt += c.PromptTokens
		totComp += c.CompletionTokens
		totReason += c.ReasoningTokens
	}

	return &ParamProbeResult{
		Protocol:              proto.ID,
		TransportMode:         mode,
		Model:                 target.Model,
		TemperatureSummary:    tempSummary,
		ThinkingSummary:       thinkSummary,
		Cases:                 allCases,
		TotalRequests:         len(allCases),
		TotalPromptTokens:     totPrompt,
		TotalCompletionTokens: totComp,
		TotalReasoningTokens:  totReason,
		DurationMs:            int(time.Since(start).Milliseconds()),
		Truncated:             truncated,
		ProbedAt:              time.Now().UTC(),
	}, nil
}

type disableCandidate struct {
	label     string
	tag       string
	overrides map[string]any
}

func defaultDisableCandidates(protocolID string) []disableCandidate {
	switch protocolID {
	case "openai-chat":
		return []disableCandidate{
			{label: "disable:default_omit", tag: "default_omit", overrides: nil},
			{label: "disable:reasoning_effort=none", tag: "reasoning_effort=none", overrides: map[string]any{"reasoning_effort": "none"}},
			{label: "disable:thinking.type=disabled", tag: "thinking.type=disabled", overrides: map[string]any{"thinking": map[string]any{"type": "disabled"}}},
			{label: "disable:enable_thinking=false", tag: "enable_thinking=false", overrides: map[string]any{"enable_thinking": false}},
		}
	case "openai-responses":
		return []disableCandidate{
			{label: "disable:default_omit", tag: "default_omit", overrides: nil},
			{label: "disable:reasoning.effort=none", tag: "reasoning.effort=none", overrides: map[string]any{"reasoning": map[string]any{"effort": "none"}}},
		}
	case "anthropic-messages":
		return []disableCandidate{
			{label: "disable:default_omit", tag: "default_omit", overrides: nil},
			{label: "disable:thinking.type=disabled", tag: "thinking.type=disabled", overrides: map[string]any{"thinking": map[string]any{"type": "disabled"}}},
		}
	default:
		return nil
	}
}

func resolveEffortCandidates(protocolID string, explicit *[]string) []string {
	var raw []string
	if explicit != nil {
		raw = *explicit
	} else if protocolID == "openai-chat" || protocolID == "openai-responses" {
		raw = []string{"low", "medium", "high", "max", "xhigh"}
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		v := strings.TrimSpace(s)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
		if len(out) >= maxParamProbeEffortCandidates {
			break
		}
	}
	return out
}

func effortOverrideForProtocol(protocolID, effort string) map[string]any {
	switch protocolID {
	case "openai-chat":
		return map[string]any{"reasoning_effort": effort}
	case "openai-responses":
		return map[string]any{"reasoning": map[string]any{"effort": effort, "summary": "auto"}}
	case "anthropic-messages":
		if n, err := strconv.Atoi(effort); err == nil && n > 0 {
			return map[string]any{"thinking": map[string]any{"type": "enabled", "budget_tokens": n}}
		}
		return nil
	default:
		return nil
	}
}

func selectProbeProtocol(target TargetConfig, protocolID string) (ProtocolConfig, adapter.Adapter, string, error) {
	var chosen *ProtocolConfig
	if strings.TrimSpace(protocolID) != "" {
		for i := range target.Protocols {
			if target.Protocols[i].ID == protocolID {
				chosen = &target.Protocols[i]
				break
			}
		}
		if chosen == nil {
			return ProtocolConfig{}, nil, "", fmt.Errorf("端点未配置指定协议 %q", protocolID)
		}
	} else {
		for i := range target.Protocols {
			if target.Protocols[i].Weight > 0 {
				chosen = &target.Protocols[i]
				break
			}
		}
		if chosen == nil {
			return ProtocolConfig{}, nil, "", fmt.Errorf("未配置任何 weight > 0 的协议")
		}
	}
	ad, ok := adapter.Get(chosen.ID)
	if !ok {
		return ProtocolConfig{}, nil, "", fmt.Errorf("未知协议适配器: %s", chosen.ID)
	}
	mode := "non_stream"
	if chosen.Transport.Stream > 0 && chosen.Capabilities.Stream {
		mode = "stream"
	}
	return *chosen, ad, mode, nil
}

func checkCustomOverridesReserved(ov map[string]any) error {
	for k := range ov {
		if config.ReservedFields[k] {
			return fmt.Errorf("body_overrides 不允许设置保留字段 %q", k)
		}
	}
	return nil
}

func cloneAnyMap(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = cloneAnyVal(v)
	}
	return out
}

func cloneAnyVal(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return cloneAnyMap(t)
	case []any:
		cp := make([]any, len(t))
		for i, x := range t {
			cp[i] = cloneAnyVal(x)
		}
		return cp
	default:
		return v
	}
}

func stripDotPath(m map[string]any, dotPath string) map[string]any {
	if len(m) == 0 || strings.TrimSpace(dotPath) == "" {
		return m
	}
	segs := strings.Split(dotPath, ".")
	cur := m
	for i := 0; i < len(segs)-1; i++ {
		next, ok := cur[segs[i]].(map[string]any)
		if !ok {
			return m
		}
		cur = next
	}
	delete(cur, segs[len(segs)-1])
	return m
}

func extractAnthropicBudgetTokens(ov map[string]any) int {
	th, ok := ov["thinking"].(map[string]any)
	if !ok {
		return 0
	}
	switch v := th["budget_tokens"].(type) {
	case int:
		return v
	case float64:
		return int(v)
	case int64:
		return int(v)
	default:
		return 0
	}
}
