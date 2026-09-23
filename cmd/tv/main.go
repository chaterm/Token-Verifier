// tv 是 Token Verifier 的命令行入口：compare（离线比较）、collect（采集）、run（采集+比较）。
package main

import (
	"fmt"
	"os"

	"github.com/chaterm/token-verifier/internal/rawdata"
)

// 退出码（README / DATAFLOW §3.6）
const (
	exitOK           = 0 // 全部探针 pass
	exitFail         = 1 // 至少一个探针 fail
	exitUsage        = 2 // 用法错误、配置非法、缺阈值
	exitIncompat     = 3 // 采集计划不兼容，拒绝比较
	exitCollect      = 4 // 采集阶段致命错误（端点不可达、续跑计划不一致、输出不可写）
	exitInconclusive = 5 // 无 fail，但存在 inconclusive 探针：判定不完整，不能被当作「全部通过」消费
	exitSubset       = 6 // 子集比较（--allow-subset）无 fail 也无 inconclusive：结论只覆盖两侧计划的交集，不是完整通过
)

func main() {
	os.Exit(run(os.Args[1:]))
}

// run 路由子命令，返回退出码。
func run(args []string) int {
	if len(args) == 0 {
		usage()
		return exitUsage
	}
	switch args[0] {
	case "compare":
		return cmdCompare(args[1:])
	case "collect":
		return cmdCollect(args[1:])
	case "run":
		return cmdRun(args[1:])
	case "-h", "--help", "help":
		usage()
		return exitOK
	case "-v", "--version", "version":
		fmt.Printf("tv %s\n", rawdata.ToolVersion)
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "ERROR  未知命令 %q\n\n", args[0])
		usage()
		return exitUsage
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `Token Verifier — 验证 LLM API 端点是否提供其声称的模型与配置

用法:
  tv compare [flags] <A.rawdata.jsonl.gz> <B.rawdata.jsonl.gz>
      读两个 rawData 文件，逐探针给出判定（完全离线）
  tv collect [flags]
      从配置的端点采集一份 rawData（联网，支持续跑）
  tv run [flags] <baseline.rawdata.jsonl.gz>
      采集 + 与基线比较一条命令完成（联网）
  tv version
      打印版本（即写进 rawData manifest 的 tool_version）

compare 的 flags:
  -c, --config <file>        配置文件（读 thresholds 段）
  --threshold <probe>=<v>    直接给阈值，可重复；优先于配置文件。
                             档位级写法 <probe>.<bucket>=<v>（如 onetoken.8000=0.12）
                             只对该 context_bucket 生效，优先于探针级
  --allow-subset             允许子集比较：两侧采集计划不同时，不再一律拒绝，
                             按字段规则调和后只比较交集（strict 字段冲突或
                             交集为空仍拒绝）。报告顶部强制印出比较范围，
                             全 pass 时退出码为 6 而非 0
  -v, --verbose              追加证据明细：逐 cell 分布直方图与传输分布对比
  --json <file>              另写 JSON 报告（始终含全量 cell 明细与直方图数据）
  --junit <file>             另写 JUnit XML 报告
  --log-level <level>        日志级别 debug|info|warn|error（默认 info）

collect 的 flags:
  -c, --config <file>        配置文件（必填）
  -o <file>                  输出 rawData 路径（必填）
  --dry-run                  只印请求数与 token 估算，不发请求
  --progress                 在 stderr 画采集进度条（显式开启；默认关闭，
                             CI 输出保持逐行日志）
  --log-level <level>        日志级别 debug|info|warn|error（默认 info）

run 的 flags:
  -c, --config <file>        配置文件（必填）
  --threshold <probe>=<v>    同 compare
  --allow-subset             同 compare（采集前预检也按子集规则放行）
  -v, --verbose              同 compare
  --json / --junit <file>    同 compare
  --keep-rawdata             保留临时 rawData（默认采集完即删）
  --progress                 在 stderr 画采集进度条（显式开启；默认关闭）
  --log-level <level>        日志级别 debug|info|warn|error（默认 info）

退出码:
  0 全部 pass · 1 有 fail · 2 用法/配置/缺阈值 · 3 采集计划不兼容
  4 采集阶段致命错误 · 5 无 fail 但有 inconclusive 探针
  6 子集比较（--allow-subset）无 fail 也无 inconclusive：结论只覆盖交集
`)
}
