package main

// collect / run 子命令：采集到 rawData（run 再与基线比较）。

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/chaterm/token-verifier/internal/collect"
	"github.com/chaterm/token-verifier/internal/compare"
	"github.com/chaterm/token-verifier/internal/config"
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
	var dryRun bool
	fs.StringVar(&configPath, "config", "", "配置文件路径（必填）")
	fs.StringVar(&configPath, "c", "", "配置文件路径（简写）")
	fs.StringVar(&outputPath, "o", "", "输出 rawData 路径（必填，如 out.rawdata.jsonl.gz）")
	fs.BoolVar(&dryRun, "dry-run", false, "只印请求数与 token 估算，不发请求")
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
	log, code := setupLogger(logLevel)
	if code != 0 {
		return code
	}

	cfg, st, code := loadCollectConfig(configPath)
	if code != 0 {
		return code
	}

	res, err := collect.Run(context.Background(), cfg, st, collect.Options{
		OutputPath: outputPath,
		DryRun:     dryRun,
		Log:        log,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR  %v\n", err)
		return exitCollect
	}
	if !dryRun {
		fmt.Fprintf(os.Stdout, "collect 完成：%d 请求（成功 %d / 失败 %d），跳过组合 %d 个\n"+
			"rawData: %s\n", res.TotalRequests, res.SuccessCount, res.ErrorCount,
			res.SkippedCount, outputPath)
	}
	return exitOK
}

// cmdRun 实现 run 子命令：采集到临时文件 → 与基线 compare → 删临时文件。
// 严格等于 collect 后接 compare，没有第二条代码路径。
func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var configPath, jsonPath, junitPath, logLevel string
	var keep, verbose bool
	thresholds := thresholdFlags{}
	fs.StringVar(&configPath, "config", "", "配置文件路径（必填，读 thresholds 段）")
	fs.StringVar(&configPath, "c", "", "配置文件路径（简写）")
	fs.Var(thresholds, "threshold", "阈值 <probe>=<value>，可重复，优先于配置文件")
	fs.StringVar(&jsonPath, "json", "", "另写 JSON 报告到该文件")
	fs.StringVar(&junitPath, "junit", "", "另写 JUnit XML 报告到该文件")
	fs.BoolVar(&verbose, "verbose", false, "追加证据明细：逐 cell 分布直方图与传输分布对比")
	fs.BoolVar(&verbose, "v", false, "同 --verbose（简写）")
	fs.BoolVar(&keep, "keep-rawdata", false, "保留临时 rawData（默认采集完即删）")
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
	log, code := setupLogger(logLevel)
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
	baseline, err := rawdata.Read(baselinePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR  %s: %v\n", baselinePath, err)
		return exitUsage
	}
	// 2. 采集计划与基线兼容（digest 相等；不等则走闸门的字段级 diff 报告）
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
	if baseline.Manifest.PlanDigest != digest {
		// 复用 compare 的闸门渲染：造一个只含 manifest 的假 File 即可
		gateErr := &compare.GateError{
			DigestA: baseline.Manifest.PlanDigest,
			DigestB: digest,
		}
		if diffs := compare.DiffDigests(baseline.Manifest.CollectionPlan, cp); diffs != nil {
			gateErr.Diffs = diffs
		}
		report.Reject(os.Stderr, gateErr)
		return exitIncompat
	}
	// 3. 待比较探针的阈值齐全（与 compare 同一套解析与校验）
	if _, code := precheckThresholds(baseline, thresholds, cfg); code != 0 {
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

	if _, err := collect.Run(context.Background(), cfg, st, collect.Options{OutputPath: tmpPath, Log: log}); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR  %v\n", err)
		return exitCollect
	}

	// 与基线比较（与 compare 完全同一条路径）
	fileB, err := rawdata.Read(tmpPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR  %v\n", err)
		return exitCollect
	}
	res, err := compare.Run(baseline, fileB, thresholds, cfg)
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

// precheckThresholds 采集前检查基线计划中所有已实现探针的阈值是否齐全合法。
// 与 compare.Run 内的解析完全同源，只是提前到花钱之前。
func precheckThresholds(baseline *rawdata.File, thresholds map[string]float64, cfg *config.File) (map[string]config.ResolvedThreshold, int) {
	planParsed, err := baseline.Manifest.CollectionPlanParsed()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR  基线采集计划解析失败: %v\n", err)
		return nil, exitUsage
	}
	var ids []string
	pValueProbes := map[string]bool{}
	for id := range planParsed.Probes {
		if p, ok := probe.Get(id); ok {
			ids = append(ids, id)
			if p.Meta().StatKind == probe.StatPValue {
				pValueProbes[id] = true
			}
		}
	}
	sort.Strings(ids)
	_, err = config.ResolveThresholds(ids, thresholds, cfg, pValueProbes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR  %v\n", err)
		return nil, exitUsage
	}
	return nil, 0
}

// verdictExitCode 从判定结果算退出码（compare/run 共用）。
// fail 优先于 inconclusive：测出差异比判定不完整更需要报警。
// inconclusive → 5：exit 0 的语义是「全部探针 pass」，带 inconclusive 的结果
// 若也返回 0，会在 CI 里被当成正常结论消费掉。
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
