package main

// collect / run 子命令：采集到 rawData（run 再与基线比较）。

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/chaterm/token-verifier/internal/collect"
	"github.com/chaterm/token-verifier/internal/compare"
	"github.com/chaterm/token-verifier/internal/config"
	"github.com/chaterm/token-verifier/internal/logging"
	"github.com/chaterm/token-verifier/internal/plan"
	"github.com/chaterm/token-verifier/internal/probe"
	"github.com/chaterm/token-verifier/internal/rawdata"
	"github.com/chaterm/token-verifier/internal/report"
	"github.com/chaterm/token-verifier/internal/suite"
)

// cmdCollect 实现 collect 子命令，返回退出码。
func cmdCollect(args []string) int {
	fs := flag.NewFlagSet("collect", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var configPath, outputPath, logLevel string
	var dryRun, progress bool
	fs.StringVar(&configPath, "config", "", "配置文件路径（必填）")
	fs.StringVar(&configPath, "c", "", "配置文件路径（简写）")
	fs.StringVar(&outputPath, "o", "", "输出 rawData 路径（必填，如 out.rawdata.jsonl.gz）")
	fs.BoolVar(&dryRun, "dry-run", false, "只印请求数与 token 估算，不发请求")
	fs.BoolVar(&progress, "progress", false, "在 stderr 画采集进度条（显式开启；默认关闭，CI 输出保持逐行日志）")
	fs.StringVar(&logLevel, "log-level", "info", "日志级别 debug|info|warn|error")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "用法: tv collect [flags]\n\nflags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if configPath == "" || outputPath == "" {
		fs.Usage()
		return exitUsage
	}

	// 进度条开启时，日志经 bar 包装的 writer 写出（写日志前先擦掉条形）
	var bar *progressBar
	logWriter := io.Writer(os.Stderr)
	if progress && !dryRun {
		bar = newProgressBar(os.Stderr, 0)
		logWriter = bar.wrapForLog(logWriter)
	}
	log, code := setupLoggerTo(logWriter, logLevel)
	if code != 0 {
		return code
	}

	cfg, st, code := loadCollectConfig(configPath)
	if code != 0 {
		return code
	}

	opts := collect.Options{OutputPath: outputPath, DryRun: dryRun, Log: log}
	if bar != nil {
		opts.Progress = func(done, total int) {
			bar.setTotal(total)
			bar.update(done)
			bar.draw()
		}
	}
	res, err := collect.Run(context.Background(), cfg, st, opts)
	if bar != nil {
		bar.finish()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR  %v\n", err)
		return exitCollect
	}
	if !dryRun {
		fmt.Fprintf(os.Stdout, "collect 完成：%d 请求（成功 %d / 失败 %d），跳过组合 %d 个\n",
			res.TotalRequests, res.SuccessCount, res.ErrorCount, res.SkippedCount)
		renderErrorBreakdown(os.Stdout, res)
		fmt.Fprintf(os.Stdout, "rawData: %s\n", outputPath)
	}
	return exitOK
}

// renderErrorBreakdown 印采集失败的分类分布、端点原话与排查方向。
// 无失败时什么都不印。错误原因此前只落进 rawData，用户在终端看不到，
// 只能从「成功 0 / 失败 131」猜 —— 这里把证据摊开。
func renderErrorBreakdown(w io.Writer, res *collect.Result) {
	if res.ErrorCount == 0 || len(res.ErrorKinds) == 0 {
		return
	}
	// 按计数降序，同数按分类名，保证输出稳定
	kinds := make([]string, 0, len(res.ErrorKinds))
	for k := range res.ErrorKinds {
		kinds = append(kinds, k)
	}
	sort.Slice(kinds, func(i, j int) bool {
		if res.ErrorKinds[kinds[i]] != res.ErrorKinds[kinds[j]] {
			return res.ErrorKinds[kinds[i]] > res.ErrorKinds[kinds[j]]
		}
		return kinds[i] < kinds[j]
	})

	fmt.Fprintf(w, "\n失败分布（%d 条）:\n", res.ErrorCount)
	for _, k := range kinds {
		s := res.ErrorSamples[k]
		head := fmt.Sprintf("  %-12s ×%d", k, res.ErrorKinds[k])
		if s.HTTPCode != 0 {
			head += fmt.Sprintf("  HTTP %d", s.HTTPCode)
		}
		fmt.Fprintln(w, head)
		if s.Detail != "" {
			// 端点返回的文本：剥控制字符后才可写终端（防 ANSI 伪造显示）
			fmt.Fprintf(w, "    端点返回: %s\n", logging.SanitizeTerminal(s.Detail))
		}
		if hint := errorKindHint(k, s.HTTPCode); hint != "" {
			fmt.Fprintf(w, "    排查: %s\n", hint)
		}
	}
	if res.SuccessCount == 0 {
		fmt.Fprintf(w, "\n注意：零成功样本 —— 这份 rawData 不含任何可比较的数据，\n"+
			"      用它做 compare 不会得到有意义的结论。\n")
	}
}

// errorKindHint 按 error_kind（4xx 再按状态码细分）给出排查方向。
// 只给指向配置项或环境的可操作提示，不猜测端点实现。
func errorKindHint(kind string, httpCode int) string {
	switch kind {
	case "connection":
		return "检查 target.base_url 是否可达、网络与代理设置"
	case "timeout":
		return "端点响应慢于 runtime.timeout_sec，可调大该值或降低 runtime.max_concurrency"
	case "protocol":
		return "请求无法按该协议组装，检查 target.protocols 的 path 与 body_overrides"
	case "parse":
		return "响应结构与协议适配器不匹配，确认 protocols[].id 与端点实际协议一致"
	case "http_3xx":
		return "端点返回重定向且未被跟随（防密钥转发到任意主机），确认 base_url 是最终地址"
	case "http_5xx":
		return "端点侧错误，非本工具配置问题；可稍后重试或联系端点提供方"
	case "http_4xx":
		switch httpCode {
		case 401, 403:
			return "认证失败，检查 target.api_key_env 指向的环境变量是否已设置且密钥有效"
		case 404:
			return "路径不存在，检查 target.base_url 与 protocols[].path 的拼接结果"
		case 429:
			return "被限流，调大 runtime.min_interval_ms 或调小 runtime.max_concurrency"
		}
		return "请求被端点拒绝，按上面的端点返回内容核对模型名与请求字段"
	}
	return ""
}

// cmdRun 实现 run 子命令：采集到临时文件 → 与基线 compare → 删临时文件。
// 严格等于 collect 后接 compare，没有第二条代码路径。
func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var configPath, jsonPath, junitPath, logLevel string
	var keep, verbose, allowSubset, progress bool
	thresholds := thresholdFlags{}
	fs.StringVar(&configPath, "config", "", "配置文件路径（必填，读 thresholds 段）")
	fs.StringVar(&configPath, "c", "", "配置文件路径（简写）")
	fs.Var(thresholds, "threshold", "阈值 <probe>=<value>，可重复，优先于配置文件")
	fs.BoolVar(&allowSubset, "allow-subset", false,
		"允许子集比较：与基线计划不同时按字段规则调和，只比较交集（strict 字段冲突或交集为空仍拒绝；全 pass 退出码为 6）")
	fs.StringVar(&jsonPath, "json", "", "另写 JSON 报告到该文件")
	fs.StringVar(&junitPath, "junit", "", "另写 JUnit XML 报告到该文件")
	fs.BoolVar(&verbose, "verbose", false, "追加证据明细：逐 cell 分布直方图与传输分布对比")
	fs.BoolVar(&verbose, "v", false, "同 --verbose（简写）")
	fs.BoolVar(&keep, "keep-rawdata", false, "保留临时 rawData（默认采集完即删）")
	fs.BoolVar(&progress, "progress", false, "在 stderr 画采集进度条（显式开启；默认关闭，CI 输出保持逐行日志）")
	fs.StringVar(&logLevel, "log-level", "info", "日志级别 debug|info|warn|error")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "用法: tv run [flags] <baseline.rawdata.jsonl.gz>\n\nflags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if configPath == "" || fs.NArg() != 1 {
		fs.Usage()
		return exitUsage
	}
	// 进度条开启时，日志经 bar 包装的 writer 写出（写日志前先擦掉条形）
	var bar *progressBar
	logWriter := io.Writer(os.Stderr)
	if progress {
		bar = newProgressBar(os.Stderr, 0)
		logWriter = bar.wrapForLog(logWriter)
	}
	log, code := setupLoggerTo(logWriter, logLevel)
	if code != 0 {
		return code
	}
	baselinePath := fs.Arg(0)

	log.Debug("run 开始", "baseline", baselinePath, "config", configPath)

	cfg, st, code := loadCollectConfig(configPath)
	if code != 0 {
		return code
	}

	// —— 采集前预检：这些检查都廉价，失败不该等到整轮采集（联网、花钱）之后 ——
	// 1. 基线文件可读
	// rawdata.Read 的错误自带文件名，这里不再重复前缀
	baseline, err := rawdata.Read(baselinePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR  %v\n", err)
		return exitUsage
	}
	// 2. 采集计划与基线兼容（digest 相等；不等则严格拒绝，或 --allow-subset
	//    下按字段三分法调和 —— 与 compare 闸门同一套规则，预检提前到花钱之前）
	cp, err := plan.Build(cfg, st)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR  %v\n", err)
		return exitUsage
	}
	digest, _, err := cp.Digest()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR  %v\n", err)
		return exitUsage
	}
	rec, code := preflightReconcile(baseline, cp, digest, allowSubset)
	if code != 0 {
		return code
	}
	// 3. 待比较探针的阈值齐全（与 compare 同一套解析与校验；子集模式下按交集档位）
	if code := precheckThresholds(rec, thresholds, cfg); code != 0 {
		return code
	}

	// 采集到临时文件
	tmpPath, err := rawdata.TempFilePath("tv-run")
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR  创建临时文件失败: %v\n", err)
		return exitCollect
	}
	defer func() {
		if !keep {
			_ = os.RemoveAll(filepath.Dir(tmpPath))
		} else {
			fmt.Fprintf(os.Stdout, "rawData 保留在: %s\n", tmpPath)
		}
	}()

	collectOpts := collect.Options{OutputPath: tmpPath, Log: log}
	if bar != nil {
		collectOpts.Progress = func(done, total int) {
			bar.setTotal(total)
			bar.update(done)
			bar.draw()
		}
	}
	collectRes, err := collect.Run(context.Background(), cfg, st, collectOpts)
	if bar != nil {
		bar.finish()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR  %v\n", err)
		return exitCollect
	}
	// 采集期的端点错误印到 stderr：stdout 留给比较报告（机器消费），
	// 但失败原因不能因此沉默 —— 它决定了下面这份报告有多少数据支撑。
	renderErrorBreakdown(os.Stderr, collectRes)

	// 与基线比较（与 compare 完全同一条路径）
	fileB, err := rawdata.Read(tmpPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR  %v\n", err)
		return exitCollect
	}
	res, err := compare.RunWith(baseline, fileB, thresholds, cfg, compare.Options{AllowSubset: allowSubset})
	var gateErr *compare.GateError
	switch {
	case errors.As(err, &gateErr):
		report.Reject(os.Stderr, gateErr)
		return exitIncompat
	case err != nil:
		fmt.Fprintf(os.Stderr, "ERROR  %v\n", err)
		return exitUsage
	}

	report.Stdout(os.Stdout, res)
	if verbose {
		report.Verbose(os.Stdout, res)
	}
	if err := writeReportFile(jsonPath, func(f *os.File) error { return report.JSON(f, res) }); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR  %v\n", err)
		return exitUsage
	}
	if err := writeReportFile(junitPath, func(f *os.File) error { return report.JUnit(f, res) }); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR  %v\n", err)
		return exitUsage
	}

	return verdictExitCode(res)
}

// preflightReconcile 采集前的计划兼容性预检（run 子命令）。
// digest 相等 → 直接放行；不等时严格模式拒绝（复用 compare 的闸门渲染），
// 子集模式按字段三分法调和 —— strict 冲突或交集为空仍拒绝。
// 返回的 *compare.Reconciled 供阈值预检按交集档位解析。
func preflightReconcile(baseline *rawdata.File, cp *plan.CollectionPlan, digest string, allowSubset bool) (*compare.Reconciled, int) {
	// format_version 不等：任何模式都拒绝（与 compare.Gate 同序）。
	// 预检补上这道检查，免得花钱采完才在比较闸门被格式版本拦下。
	if baseline.Manifest.FormatVersion != rawdata.FormatVersion {
		report.Reject(os.Stderr, &compare.GateError{
			DigestA:        baseline.Manifest.PlanDigest,
			DigestB:        digest,
			FormatVersionA: baseline.Manifest.FormatVersion,
			FormatVersionB: rawdata.FormatVersion,
		})
		return nil, exitIncompat
	}
	if baseline.Manifest.PlanDigest == digest {
		planParsed, err := baseline.Manifest.CollectionPlanParsed()
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR  基线采集计划解析失败: %v\n", err)
			return nil, exitUsage
		}
		return compare.ReconciledFromPlan(planParsed, digest), 0
	}

	// 复用 compare 的闸门渲染：造一个只含 digest 与 diff 的 GateError
	gateErr := &compare.GateError{
		DigestA:        baseline.Manifest.PlanDigest,
		DigestB:        digest,
		FormatVersionA: baseline.Manifest.FormatVersion,
		FormatVersionB: rawdata.FormatVersion,
	}
	if diffs := compare.DiffDigests(baseline.Manifest.CollectionPlan, cp); diffs != nil {
		gateErr.Diffs = diffs
	}
	if !allowSubset {
		report.Reject(os.Stderr, gateErr)
		return nil, exitIncompat
	}

	// 子集模式：与 compare.Gate 同一套调和规则（本次计划尚未落盘，经 JSON 往返）
	rawCP, err := json.Marshal(cp)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR  采集计划序列化失败: %v\n", err)
		return nil, exitUsage
	}
	rec, conflicts, err := compare.ReconcilePlans(baseline.Manifest.CollectionPlan, rawCP,
		baseline.Manifest.PlanDigest, digest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR  %v\n", err)
		return nil, exitUsage
	}
	if len(conflicts) > 0 {
		gateErr.Diffs = conflicts
		gateErr.Subset = true
		report.Reject(os.Stderr, gateErr)
		return nil, exitIncompat
	}
	if len(rec.ProbeIDs) == 0 {
		gateErr.Subset = true
		report.Reject(os.Stderr, gateErr)
		return nil, exitIncompat
	}
	return rec, 0
}

// precheckThresholds 采集前检查调和后计划中所有已实现探针的阈值是否齐全合法。
// 与 compare.RunWith 内的解析完全同源（含档位级阈值；子集模式下按交集档位），
// 只是提前到花钱之前。
func precheckThresholds(rec *compare.Reconciled, thresholds map[string]float64, cfg *config.File) int {
	pValueProbes := map[string]bool{}
	probeBuckets := map[string][]int{}
	for _, id := range rec.ProbeIDs {
		pp := rec.Probes[id]
		if p, ok := probe.Get(id); ok {
			if p.Meta().StatKind == probe.StatPValue {
				pValueProbes[id] = true
			}
			// 与 compare.RunWith 同构：cell_key 不含 context_bucket 维度时
			// 只要求探针级阈值（退化路径）
			probeBuckets[id] = nil
			if compare.BucketedCellKey(pp.CellKey) {
				probeBuckets[id] = pp.ContextBuckets
			}
		}
	}
	_, err := config.ResolveThresholds(probeBuckets, thresholds, cfg, pValueProbes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR  %v\n", err)
		return exitUsage
	}
	return 0
}

// verdictExitCode 从判定结果算退出码（compare/run 共用）。
// fail 优先于 inconclusive：测出差异比判定不完整更需要报警。
// inconclusive → 5：exit 0 的语义是「全部探针 pass」，带 inconclusive 的结果
// 若也返回 0，会在 CI 里被当成正常结论消费掉。
// 子集比较 → 6：理由与 5 同构 —— exit 0 的语义是「全部探针 pass」，
// 一份只覆盖交集的结果若也返回 0，缩水的范围会在 CI 里被当成完整通过。
func verdictExitCode(res *compare.Result) int {
	hasFail, hasInconclusive := false, false
	for _, v := range res.Verdicts {
		switch v.Verdict {
		case "fail":
			hasFail = true
		case "inconclusive":
			hasInconclusive = true
		}
	}
	switch {
	case hasFail:
		return exitFail
	case hasInconclusive:
		return exitInconclusive
	case res.Scope != nil:
		return exitSubset
	}
	return exitOK
}

// loadCollectConfig 加载并校验采集所需配置与题库（collect/run 共用）。
func loadCollectConfig(configPath string) (*config.File, *suite.File, int) {
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR  %v\n", err)
		return nil, nil, exitUsage
	}
	// 结构性校验前移到加载题库之前：否则配置缺 suite.path 时，suite.Load 会拿
	// 空路径去读文件，报出 `open : The system cannot find the file specified.`
	// —— 把「配置缺字段」伪装成「题库文件不存在」。（Validate 内部仍会先跑这一步）
	if err := cfg.ValidateStructure(); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR  %v\n", err)
		return nil, nil, exitUsage
	}
	st, err := suite.Load(cfg.Suite.Path, cfg.Suite.SHA256)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR  %v\n", err)
		return nil, nil, exitUsage
	}
	// 校验用的题目信息
	items := map[string][]config.SuiteItemInfo{}
	for _, id := range cfg.EnabledProbes() {
		ps, ok := st.ProbeSetOf(id)
		if !ok {
			continue
		}
		for _, it := range ps.Items {
			eff := ps.ThinkingEffortOf(it)
			items[id] = append(items[id], config.SuiteItemInfo{ID: it.ID, ThinkingEffort: eff})
		}
	}
	err = cfg.Validate(items, func(msg string) {
		fmt.Fprintf(os.Stderr, "WARN  %s\n", msg)
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR  %v\n", err)
		return nil, nil, exitUsage
	}
	return cfg, st, 0
}
