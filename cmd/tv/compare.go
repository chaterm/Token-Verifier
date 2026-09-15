package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/chaterm/token-verifier/internal/compare"
	"github.com/chaterm/token-verifier/internal/config"
	"github.com/chaterm/token-verifier/internal/logging"
	"github.com/chaterm/token-verifier/internal/rawdata"
	"github.com/chaterm/token-verifier/internal/report"
)

// thresholdFlags 收集可重复的 --threshold <probe>=<value>。
type thresholdFlags map[string]float64

func (t thresholdFlags) String() string { return "" }

func (t thresholdFlags) Set(s string) error {
	id, val, err := config.ParseThresholdFlag(s)
	if err != nil {
		return err
	}
	t[id] = val
	return nil
}

// setupLogger 解析 --log-level 并构造写到 stderr 的 logger（三个子命令共用）。
// 级别非法时报错并返回 exitUsage。
func setupLogger(levelName string) (*slog.Logger, int) {
	level, err := logging.ParseLevel(levelName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR  %v\n", err)
		return nil, exitUsage
	}
	return logging.Setup(os.Stderr, level), 0
}

// cmdCompare 实现 compare 子命令，返回退出码。
func cmdCompare(args []string) int {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var configPath, jsonPath, junitPath, logLevel string
	var verbose, allowSubset bool
	thresholds := thresholdFlags{}
	fs.StringVar(&configPath, "config", "", "配置文件路径（读 thresholds 段）")
	fs.StringVar(&configPath, "c", "", "配置文件路径（简写）")
	fs.Var(thresholds, "threshold", "阈值 <probe>=<value>，可重复，优先于配置文件")
	fs.BoolVar(&allowSubset, "allow-subset", false,
		"允许子集比较：计划不同时按字段规则调和，只比较交集（strict 字段冲突或交集为空仍拒绝；全 pass 退出码为 6）")
	fs.StringVar(&jsonPath, "json", "", "另写 JSON 报告到该文件")
	fs.StringVar(&junitPath, "junit", "", "另写 JUnit XML 报告到该文件")
	fs.BoolVar(&verbose, "verbose", false, "追加证据明细：逐 cell 分布直方图与传输分布对比")
	fs.BoolVar(&verbose, "v", false, "同 --verbose（简写）")
	fs.StringVar(&logLevel, "log-level", "info", "日志级别 debug|info|warn|error")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "用法: tv compare [flags] <A.rawdata.jsonl.gz> <B.rawdata.jsonl.gz>\n\nflags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return exitUsage
	}
	log, code := setupLogger(logLevel)
	if code != 0 {
		return code
	}
	log.Debug("compare 开始", "file_a", fs.Arg(0), "file_b", fs.Arg(1))

	pathA, pathB := fs.Arg(0), fs.Arg(1)
	fileA, err := rawdata.Read(pathA)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR  %s: %v\n", pathA, err)
		return exitUsage
	}
	fileB, err := rawdata.Read(pathB)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR  %s: %v\n", pathB, err)
		return exitUsage
	}

	// 配置文件可选：不传 -c 时阈值只能来自 --threshold
	var cfg *config.File
	if configPath != "" {
		cfg, err = config.Load(configPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR  %v\n", err)
			return exitUsage
		}
	}

	res, err := compare.RunWith(fileA, fileB, thresholds, cfg, compare.Options{AllowSubset: allowSubset})
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

	// fail 优先于 inconclusive（理由见 verdictExitCode）
	return verdictExitCode(res)
}

// writeReportFile 路径为空则跳过；否则创建文件并写入。
func writeReportFile(path string, write func(*os.File) error) error {
	if path == "" {
		return nil
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("创建报告文件 %s 失败: %w", path, err)
	}
	defer f.Close()
	return write(f)
}
