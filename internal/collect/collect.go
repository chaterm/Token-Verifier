// collect 包编排采集：构建计划 → 比例分配 → 能力裁剪 → 洗牌 → 执行 → 落盘。
package collect

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chaterm/token-verifier/internal/adapter"
	"github.com/chaterm/token-verifier/internal/config"
	"github.com/chaterm/token-verifier/internal/logging"
	"github.com/chaterm/token-verifier/internal/plan"
	"github.com/chaterm/token-verifier/internal/rawdata"
	"github.com/chaterm/token-verifier/internal/suite"
	"github.com/chaterm/token-verifier/internal/transport"
)

// Options 采集选项（命令行层填）。
type Options struct {
	OutputPath string // rawData 输出路径
	DryRun     bool   // 只印请求数与 token 估算
	// Log 进度日志；nil = 静默（库式调用不打日志）
	Log *slog.Logger
	// Progress 每个任务（不是每次尝试）完成后回调，done 单调递增、终值等于
	// total（= 本次待发的请求数，不含续跑已完成的部分）。nil = 无进度输出。
	// 回调可能从多个 goroutine 并发触发，实现方需自行同步。
	Progress func(done, total int)
}

// Result 采集结果摘要。
type Result struct {
	TotalRequests int
	SuccessCount  int
	ErrorCount    int
	SkippedCount  int
	// ErrorKinds 失败按 error_kind 的计数（connection / timeout / protocol /
	// parse / http_3xx / http_4xx / http_5xx）。分类本身在 transport 层已经很细，
	// 但此前只落盘不出口，用户看不到「为什么失败」。
	ErrorKinds map[string]int
	// ErrorSamples 每种 error_kind 的首例详情，供 CLI 印出端点原话。
	// 只留首例：131 个请求可能有 131 条不同 detail，全留会把摘要冲爆。
	ErrorSamples map[string]ErrorSample
}

// ErrorSample 一种 error_kind 的首例证据。
type ErrorSample struct {
	HTTPCode int    // 0 = 未收到响应（连接失败/超时）
	Detail   string // 已脱敏的端点原话（未剥控制字符，渲染前须过 SanitizeTerminal）
	ProbeID  string
	Protocol string
}

// Run 执行一次采集。cfg 必须已通过 Validate。
func Run(ctx context.Context, cfg *config.File, st *suite.File, opts Options) (*Result, error) {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	// 1. 构建采集计划 + digest
	cp, err := plan.Build(cfg, st)
	if err != nil {
		return nil, err
	}
	digest, canonical, err := cp.Digest()
	if err != nil {
		return nil, err
	}

	// 2. 逐 cell 分配 + 能力裁剪 + 任务展开
	norm := st.Normalizer()
	tasks, skips, sampling, err := expandTasks(cfg, st, cp, norm)
	if err != nil {
		return nil, err
	}
	skips = dedupSkips(skips)

	// 3. seed 洗牌（只影响发出顺序，不影响样本数；不进 digest）
	r := rand.New(rand.NewSource(cfg.Runtime.Seed))
	r.Shuffle(len(tasks), func(i, j int) { tasks[i], tasks[j] = tasks[j], tasks[i] })

	if opts.DryRun {
		return dryRunSummary(cp, tasks), nil
	}

	// 4. manifest + 续跑读回（文件即自身续跑状态）
	manifest, err := buildManifest(cfg, cp, canonical, digest, skips, sampling)
	if err != nil {
		return nil, err
	}
	done, err := rawdata.EnsureFile(opts.OutputPath, manifest)
	if err != nil {
		return nil, err
	}
	// 过滤已完成的请求
	var pending []task
	for _, t := range tasks {
		if !done[rawdata.ResumeKey{
			ProbeID: t.probeID, QuestionID: t.questionID, ContextBucket: t.bucket,
			Protocol: t.protocol, TransportMode: t.transportMode, RepeatIndex: t.repeat,
		}] {
			pending = append(pending, t)
		}
	}

	res := &Result{TotalRequests: len(pending), SkippedCount: len(skips)}
	log.Info("开始采集",
		"requests", len(pending),
		"resumed", len(tasks)-len(pending),
		"skipped_combos", len(skips),
		"output", opts.OutputPath)
	if len(pending) == 0 {
		// 全部已完成：直接收尾 aggregates
		return res, finalize(opts.OutputPath)
	}

	// 5. 执行：record 边采边落盘（每条 Flush），中途崩溃已完成部分不丢
	w, err := rawdata.Append(opts.OutputPath)
	if err != nil {
		return res, err
	}
	writeClosed := false
	closeWriter := func() error {
		if writeClosed {
			return nil
		}
		writeClosed = true
		return w.Close()
	}
	// 兜底关闭：正常路径里收尾会显式 closeWriter 并处理错误，
	// 这里只保证异常退出时文件句柄不泄漏
	defer func() { _ = closeWriter() }()

	apiKey := os.Getenv(cfg.Target.APIKeyEnv)
	client := transport.New(transport.Options{
		MaxConcurrency: cfg.Runtime.MaxConcurrency,
		TimeoutSec:     cfg.Runtime.TimeoutSec,
		MinIntervalMs:  cfg.Runtime.MinIntervalMs,
		MaxAttempts:    cfg.Runtime.MaxAttempts,
		BackoffBaseMs:  cfg.Runtime.BackoffBaseMs,
		BackoffMaxMs:   cfg.Runtime.BackoffMaxMs,
	})
	runner := transport.NewRunner(client)
	if opts.Progress != nil {
		// 任务粒度进度：done 单调递增，重试不重复计（OnTaskDone 每任务恰好一次）
		var done atomic.Int64
		total := len(pending)
		runner.OnTaskDone = func(int) {
			opts.Progress(int(done.Add(1)), total)
		}
	}

	tt := make([]transport.Task, len(pending))
	for i, t := range pending {
		tt[i] = transport.Task{
			Adapter:       t.adapter,
			BaseURL:       cfg.Target.BaseURL,
			APIKey:        apiKey,
			Logical:       t.logical,
			Thinking:      t.thinking,
			TransportMode: t.transportMode,
			RedactSecret:  apiKey,
		}
	}

	// 端点可达性：收到过任何 HTTP 响应（含 4xx/5xx）即为可达
	var gotAnyResponse atomic.Bool
	var success, failed atomic.Int64
	var mu sync.Mutex // 保护 writer 与错误账本（回调来自多个 goroutine）
	// 错误账本：分类计数 + 每类首例。均在 mu 下读写。
	errorKinds := map[string]int{}
	errorSamples := map[string]ErrorSample{}

	runner.Run(ctx, tt, func(idx int, attempt int, tr transport.Result) {
		t := pending[idx]
		rec := rawdata.Record{
			ProbeID:        t.probeID,
			QuestionID:     t.questionID,
			ContextBucket:  t.bucket,
			Protocol:       t.protocol,
			TransportMode:  t.transportMode,
			ThinkingEffort: t.thinkingEffort,
			RepeatIndex:    t.repeat,
			Attempt:        attempt,
		}
		transport.FillTransport(&rec, t.transportMode, tr)
		if tr.HTTPCode != 0 {
			gotAnyResponse.Store(true)
		}
		if tr.ErrorKind == "" {
			// Observe 前移：观测值在采集期提取，digest 档不落原文也能算全部统计
			obs, oerr := observeFor(t.probeID, tr.Response, t.norm, t.normalizeRule, t.contractField,
				t.needleMarkers, t.toolNames, t.reasoningUnit)
			if oerr != nil {
				rec.Status = "error"
				kind := "parse"
				rec.ErrorKind = &kind
				detail := oerr.Error()
				rec.ErrorDetail = &detail
			} else {
				rec.Observation = obs
				rec.RawContentSHA256 = sha256Hex(tr.Response.Content)
				rec.Usage = rawdata.Usage{
					Prompt:     tr.Response.Usage.Prompt,
					Completion: tr.Response.Usage.Completion,
					Reasoning:  tr.Response.Usage.Reasoning,
					Cached:     tr.Response.Usage.Cached,
				}
			}
		}

		mu.Lock()
		defer mu.Unlock()
		// debug 行带上失败原因：此前只有 status=error 而无 error_kind/detail，
		// 开到最详细也看不出端点到底报了什么。
		debugArgs := []any{
			"probe", t.probeID, "question", t.questionID, "bucket", t.bucket,
			"protocol", t.protocol, "mode", t.transportMode, "repeat", t.repeat,
			"attempt", attempt, "status", rec.Status,
			"http_code", tr.HTTPCode, "latency_ms", tr.LatencyMs,
		}
		if rec.ErrorKind != nil {
			debugArgs = append(debugArgs, "error_kind", *rec.ErrorKind)
		}
		if rec.ErrorDetail != nil {
			debugArgs = append(debugArgs, "error_detail",
				logging.SanitizeTerminal(truncateForTerminal(*rec.ErrorDetail)))
		}
		log.Debug("请求完成", debugArgs...)

		// 失败即时上报：每种 error_kind 的首例当场打一条 WARN（默认级别可见），
		// 让用户在第一个请求就能止损，而不是等整轮采集跑完（上百请求、要花钱）
		// 才从「成功 0 / 失败 131」里猜原因。同类只报一次，避免刷屏。
		if rec.ErrorKind != nil {
			kind := *rec.ErrorKind
			errorKinds[kind]++
			if _, seen := errorSamples[kind]; !seen {
				detail := ""
				if rec.ErrorDetail != nil {
					detail = *rec.ErrorDetail
				}
				code := 0
				if rec.HTTPCode != nil {
					code = *rec.HTTPCode
				}
				errorSamples[kind] = ErrorSample{
					HTTPCode: code, Detail: detail,
					ProbeID: t.probeID, Protocol: t.protocol,
				}
				warnArgs := []any{"error_kind", kind}
				if code != 0 {
					warnArgs = append(warnArgs, "http_code", code)
				}
				warnArgs = append(warnArgs, "probe", t.probeID, "protocol", t.protocol)
				if detail != "" {
					// 端点返回的文本：剥控制字符后才可写终端（防 ANSI 伪造显示）
					warnArgs = append(warnArgs, "detail",
						logging.SanitizeTerminal(truncateForTerminal(detail)))
				}
				warnArgs = append(warnArgs, "note", "同类错误后续不再逐条打印")
				log.Warn("请求失败", warnArgs...)
			}
		}

		err := w.WriteRecord(rec)
		if err == nil {
			err = w.Flush()
		}
		if err != nil {
			// 落盘失败：记数并继续，收尾时统一报（丢的是本条，不中断采集）
			failed.Add(1)
			log.Warn("record 落盘失败", "err", err)
			return
		}
		if rec.Status == "success" {
			success.Add(1)
		} else {
			failed.Add(1)
		}
	})

	res.SuccessCount = int(success.Load())
	res.ErrorCount = int(failed.Load())
	mu.Lock()
	res.ErrorKinds, res.ErrorSamples = errorKinds, errorSamples
	mu.Unlock()

	// 6. 采集致命错误边界：一个成功样本都没有且从未收到任何 HTTP 响应
	// = 端点完全不可达。个别请求失败、全 4xx/5xx 都不算 —— 那些是数据本身。
	if res.SuccessCount == 0 && !gotAnyResponse.Load() {
		return nil, fmt.Errorf("端点不可达：%d 个请求全部连接失败，未收到任何 HTTP 响应", len(pending))
	}

	if err := closeWriter(); err != nil {
		return res, err
	}

	// 7. 读回全部 record（含历史批次）重算 aggregates 并收尾
	return res, finalize(opts.OutputPath)
}

// finalize 读回文件重算 aggregates 追加到末尾。
func finalize(path string) error {
	f, err := rawdata.Read(path)
	if err != nil {
		return fmt.Errorf("收尾读回失败: %w", err)
	}
	return rawdata.WriteAggregates(path, f.Records)
}

// task 一次具体请求的全部定位信息。
type task struct {
	probeID        string
	questionID     string
	bucket         int
	protocol       string
	transportMode  string
	thinkingEffort *string
	repeat         int
	normalizeRule  string
	contractField  string // 输出契约的答案字段名；空 = raw（不做 JSON 解包）
	norm           *suite.Normalizer
	adapter        adapter.Adapter
	logical        adapter.LogicalRequest
	thinking       adapter.ThinkingBlock
	// needle：该题全部埋点的标记（下标 = 位置序号），观测时逐标记判包含
	needleMarkers []string
	// toolcall：生效工具集的工具名（args_valid 判定用）
	toolNames []string
	// think-effort：该协议的思考量观测单位（adapter.ReasoningUnit）
	reasoningUnit string
}

// expandTasks 按 探针×题目×档位 展开 cell，逐 cell 分配并生成任务。
// 返回任务、能力裁剪产生的 skipped、逐探针分配表（manifest.sampling）。
func expandTasks(cfg *config.File, st *suite.File, cp *plan.CollectionPlan, norm *suite.Normalizer) ([]task, []rawdata.Skip, map[string]map[string]int, error) {
	var tasks []task
	var skips []rawdata.Skip
	// sampling：逐探针的 (protocol/transport) → 整数计数（SPEC-RAWDATA §2）
	sampling := map[string]map[string]int{}

	for _, probeID := range cfg.EnabledProbes() {
		pp, ok := cp.Probes[probeID]
		if !ok {
			// 启用但本版本未实现的探针：跳过并记录
			skips = append(skips, rawdata.Skip{
				ProbeID: probeID,
				Reason:  "not_implemented",
				Detail:  "本版本尚未实现该探针的采集",
			})
			continue
		}
		ps, _ := st.ProbeSetOf(probeID)
		probeSampling := map[string]int{}
		for _, item := range ps.Items {
			// 输出契约机械规则：生效 normalize ≠ none → 注入 system prompt 强约束，
			// 观测端按契约字段做 JSON 解包（SPEC-SUITE）。
			normalizeRule := ps.NormalizeOf(item)
			contract := st.ContractFor(normalizeRule)
			var systemPrompt, contractField string
			if contract != nil {
				systemPrompt = contract.SystemPrompt
				contractField = contract.FieldOf()
			}
			// needle：埋点位置与派生标记（题库只声明位置，标记不落明文）
			var needleMarkers []string
			var needlePositions []float64
			if probeID == "needle" {
				needlePositions = ps.PositionsOf(item)
				needleMarkers = suite.NeedleMarkers(item.ID, needlePositions)
			}
			// toolcall：生效工具集（题目级覆盖探针级）
			var toolDefs []adapter.ToolDef
			var toolNames []string
			if probeID == "toolcall" {
				for _, t := range ps.ToolsOf(item) {
					toolDefs = append(toolDefs, adapter.ToolDef{
						Name: t.Name, Description: t.Description, Parameters: t.Parameters,
					})
					toolNames = append(toolNames, t.Name)
				}
			}
			effort := ps.ThinkingEffortOf(item)
			for _, bucket := range pp.ContextBuckets {
				alloc := AllocateProbe(probeID, bucket, pp.Repeats, cfg.Target.Protocols,
					probeID == "toolcall", probeID == "think-effort" || effort != "")
				skips = append(skips, alloc.Skipped...)
				for _, combo := range alloc.Keys() {
					protoID, mode := combo[0], combo[1]
					n := alloc.CountOf(protoID, mode)
					probeSampling[protoID+"/"+mode] += n
					ad, ok := adapter.Get(protoID)
					if !ok {
						skips = append(skips, rawdata.Skip{
							ProbeID: probeID, Protocol: protoID,
							Reason: "adapter_missing",
							Detail: "本版本未实现该协议适配器",
						})
						continue
					}
					proto := protocolByID(cfg, protoID)
					tb, err := resolveInjection(cfg, proto, effort)
					if err != nil {
						return nil, nil, nil, fmt.Errorf("探针 %s 题目 %s: %w", probeID, item.ID, err)
					}
					var effPtr *string
					if effort != "" {
						e := effort
						effPtr = &e
					}
					// needle：标记埋入该 bucket 的填充语料，作为 Padding 前缀；
					// 其余探针 Padding 就是裸语料。位置按原始语料长度计，
					// 同一 (bucket, seed) 恒得同一文本，跨侧一致由 digest 闸门承接。
					padding := suite.GeneratePadding(bucket, cp.PaddingSeed)
					if probeID == "needle" {
						padding = suite.AssembleNeedleContext(padding, item.ID, needlePositions)
					}
					for i := 0; i < n; i++ {
						lr := adapter.LogicalRequest{
							Model:            cfg.Target.Model,
							System:           systemPrompt,
							Prompt:           item.Prompt,
							Padding:          padding,
							MaxTokens:        pp.MaxTokens,
							Temperature:      pp.Temperature,
							TopP:             pp.TopP,
							Stream:           mode == "stream",
							Path:             proto.Path,
							TemperatureField: cfg.Target.Request.TemperatureField,
							Tools:            toolDefs,
						}
						tasks = append(tasks, task{
							probeID: probeID, questionID: item.ID, bucket: bucket,
							protocol: protoID, transportMode: mode,
							thinkingEffort: effPtr, repeat: i,
							normalizeRule: normalizeRule,
							contractField: contractField,
							norm:          norm,
							adapter:       ad, logical: lr, thinking: tb,
							needleMarkers: needleMarkers, toolNames: toolNames,
							reasoningUnit: adapter.ReasoningUnit(protoID),
						})
					}
				}
			}
		}
		sampling[probeID] = probeSampling
	}
	return tasks, skips, sampling, nil
}

// resolveInjection 组装请求体注入（SPEC-CONFIG §4.2/§3.4）：
// target.request.body_overrides → thinking 块 body_overrides（后者覆盖前者），
// 再展开 ${thinking_effort} 占位符（类型保留）。
// effort 为空串（null）→ disabled 块；有级别 → enabled 块 + effort_map 映射。
func resolveInjection(cfg *config.File, proto config.ProtocolConfig, effort string) (adapter.ThinkingBlock, error) {
	global := cfg.Target.Request.BodyOverrides
	var thinkingOV map[string]any
	if effort == "" {
		thinkingOV = proto.Thinking.Disabled.BodyOverrides // 缺块视为空对象
	} else {
		enabled := proto.Thinking.Enabled
		mapped, ok := enabled.EffortMap[effort]
		if !ok {
			// 配置校验应已拦住；这里是兜底
			return adapter.ThinkingBlock{}, fmt.Errorf("协议 %s 的 effort_map 缺少级别 %q", proto.ID, effort)
		}
		thinkingOV = expandEffort(enabled.BodyOverrides, mapped)
	}
	merged := adapter.DeepMerge(global, thinkingOV)
	return adapter.ThinkingBlock{BodyOverrides: merged}, nil
}

// expandEffort 展开 body_overrides 里的 ${thinking_effort}（递归、类型保留）。
func expandEffort(ov map[string]any, mapped any) map[string]any {
	if ov == nil {
		return nil
	}
	return adapter.ExpandPlaceholders(ov, mapped).(map[string]any)
}

// dedupSkips 去掉重复的 skip 条目：分配按 题×档位 循环，
// 同一 (probe, protocol, reason) 的能力裁剪会重复出现，记录一次即可。
func dedupSkips(skips []rawdata.Skip) []rawdata.Skip {
	seen := map[rawdata.Skip]bool{}
	out := make([]rawdata.Skip, 0, len(skips))
	for _, s := range skips {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// protocolByID 从配置找协议；找不到返回零值（expandTasks 已保证存在）。
func protocolByID(cfg *config.File, id string) config.ProtocolConfig {
	for _, p := range cfg.Target.Protocols {
		if p.ID == id {
			return p
		}
	}
	return config.ProtocolConfig{}
}

// buildManifest 组装 manifest：endpoint 只放密钥与主机名的指纹，明文均不落盘。
func buildManifest(cfg *config.File, cp *plan.CollectionPlan, canonical []byte, digest string, skips []rawdata.Skip, sampling map[string]map[string]int) (rawdata.Manifest, error) {
	apiKey := os.Getenv(cfg.Target.APIKeyEnv)
	sum := sha256.Sum256([]byte(apiKey))
	fingerprint := hex.EncodeToString(sum[:])[:8]

	// capabilities：逐协议的能力对象（SPEC-RAWDATA §2 形态）
	caps := map[string]config.Capabilities{}
	for _, p := range cfg.Target.Protocols {
		caps[p.ID] = p.Capabilities
	}
	capsJSON, err := json.Marshal(caps)
	if err != nil {
		return rawdata.Manifest{}, err
	}

	samplingJSON, err := json.Marshal(sampling)
	if err != nil {
		return rawdata.Manifest{}, err
	}

	host := cfg.Target.BaseURL
	if u, err := url.Parse(cfg.Target.BaseURL); err == nil && u.Host != "" {
		host = u.Host
	}
	hostSum := sha256.Sum256([]byte(host))
	hostFingerprint := hex.EncodeToString(hostSum[:])[:16]

	probeDigests := map[string]string{}
	for id := range cp.Probes {
		if d, err := cp.ProbeDigest(id); err == nil {
			probeDigests[id] = d
		}
	}

	return rawdata.Manifest{
		Kind:           "manifest",
		FormatVersion:  rawdata.FormatVersion,
		ToolVersion:    rawdata.ToolVersion,
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
		CollectionPlan: json.RawMessage(canonical),
		PlanDigest:     digest,
		ProbeDigests:   probeDigests,
		Endpoint: rawdata.Endpoint{
			Model:              cfg.Target.Model,
			APIKeyFingerprint:  fingerprint,
			BaseURLFingerprint: hostFingerprint,
		},
		Capabilities: capsJSON,
		Skipped:      skips,
		Sampling:     samplingJSON,
		RawLevel:     cfg.Runtime.RawLevel,
	}, nil
}

// dryRunSummary 印请求数与 token 估算（不落盘不联网）。
func dryRunSummary(cp *plan.CollectionPlan, tasks []task) *Result {
	res := &Result{TotalRequests: len(tasks)}
	var estPromptTokens int
	for _, t := range tasks {
		// 粗估：prompt 字符数 / 3（中英混合近似）；max_tokens 为输出上限
		estPromptTokens += (len(t.logical.Prompt) + len(t.logical.Padding)) / 3
		if t.logical.MaxTokens != nil {
			estPromptTokens += *t.logical.MaxTokens
		}
	}
	fmt.Printf("dry-run: %d 个请求，token 估算 ≈ %d（输入按字符/3 估，输出按 max_tokens 上限估）\n",
		res.TotalRequests, estPromptTokens)
	for id, pp := range cp.Probes {
		n := 0
		for _, t := range tasks {
			if t.probeID == id {
				n++
			}
		}
		fmt.Printf("  %-12s %d 请求（%d 题 × %d 档位 × repeats=%d）\n", id, n, len(pp.QuestionIDs), len(pp.ContextBuckets), pp.Repeats)
	}
	return res
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// terminalDetailMaxRunes 单条日志里错误详情的展示上限。落盘的上限是 4KiB
// （transport.capDetail），那个长度塞进一行日志会把终端冲掉。
const terminalDetailMaxRunes = 200

// truncateForTerminal 按 rune 截断到 terminalDetailMaxRunes，不切碎多字节字符。
func truncateForTerminal(s string) string {
	rs := []rune(s)
	if len(rs) <= terminalDetailMaxRunes {
		return s
	}
	return string(rs[:terminalDetailMaxRunes]) + "…"
}
