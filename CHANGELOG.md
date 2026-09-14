# Changelog

所有显著变更记录于此。观测语义变更（影响 rawData 可比性的）用 **粗体** 标注。

格式基于 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

## [Unreleased]

### Added

- GitHub Actions CI：gofmt / go vet / go test 三平台矩阵 + golangci-lint
- goreleaser 发布配置：多平台二进制内置示例配置与题库，`v*` tag 自动出 draft release
- `Publish Baseline` workflow：题库 + 官方 rawData 成对发布（强制 digest 级门禁）。
  随后迁移至独立数据仓库 [Token-Verifier-Data](https://github.com/chaterm/Token-Verifier-Data)
  （draft release 门禁 + config `suite.path`/`sha256` 自动改写），本仓库不再随附
- 社区文件：CONTRIBUTING / CODE_OF_CONDUCT / SECURITY
- docs/BASELINES.md 官方基线索引
- 档位级阈值：`thresholds` 段与 `--threshold` 支持 `<probe>.<bucket>` 键
  （如 `onetoken.8000: 0.12`、`--threshold onetoken.8000=0.12`），只对指定
  `context_bucket` 生效，未覆盖档位回退探针级值。档位须在探针的
  `context_buckets` 内，笔误（如 `onetoken.8001`）在配置校验与比较入口
  直接报错。优先级：flag 档位 > config 档位 > flag 探针级 > config 探针级
- 档位粒度判定与报告：多档位探针按 `context_bucket` 分区、各自判定，
  报告逐档位展开 —— stdout 档位子行、JSON `probes[].buckets` 数组、
  verbose 档位小标题、JUnit 每档位一个 testcase
  （`onetoken[context_bucket=8000]`）

### Changed

- 判定语义变更（破坏性，不影响 rawData 可比性）：多档位探针从
  「跨档位聚合后比一次」改为「每档位独立判定、探针级取最差档位」。
  即使不写档位级阈值，结论也可能变化：例如 onetoken 两档 JSD 0.05 / 0.19、
  阈值 0.15，旧行为均值 0.12 → PASS，新行为最差 0.19 → FAIL。
  p 值类探针（tokenizer / needle / think-effort）单档位检验样本量下降、
  功效降低，B 个档位的探针级族错误率约 `1-(1-α)^B`；需要维持旧宽松度时
  调高阈值或按档位单独标定。阈值不进 digest，已有 rawData 无需重采
- JUnit 报告：多档位探针的 testcase 从每探针一个变为每档位一个，
  `tests` / `failures` / `skipped` 计数随档位展开；按测试名做趋势的 CI 会看到
  `onetoken[context_bucket=N]` 形式的新名字。单档位探针名字不变
- openai-chat 流式请求默认携带 `stream_options: {include_usage: true}`
  （Chat Completions 流式不显式开启时服务端不上报 usage，观测会缺 token 数）。
  非保留字段，端点不识别时可经 `body_overrides` 覆盖
- **`ToolVersion` 从常量 `0.2.0` 改为由 ldflags 注入（源码构建时为 `dev`）**：发布版 rawData manifest 的 tool_version 将与 git tag 一致
- 示例配置改为模型无关占位；示例题库精简为 `suites/v1.example.yaml`
