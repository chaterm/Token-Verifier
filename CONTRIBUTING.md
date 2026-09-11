# 贡献指南

感谢关注 Token-Verifier！这个工具的核心价值在于**观测语义的可复现性**——贡献时请先阅读 [docs/SPEC-RAWDATA.md](./docs/SPEC-RAWDATA.md) 理解 digest 闸门的含义，再动代码。

## 开发环境

- Go 1.25+
- 本地验证：`gofmt -l .`（应无输出）、`go vet ./...`、`go test -race ./...`
- CI（GitHub Actions）会在 Linux/macOS/Windows 三平台跑同样检查，golangci-lint 必须零告警

## Pull Request 流程

1. Fork + 分支开发，PR 目标分支为 `dev`
2. PR 描述写清动机与行为变化；影响观测语义的改动必须说明（见下节）
3. 全部 CI 通过后由维护者合并

## 观测语义变更的硬规则

这是本项目区别于普通工具的约束，违反会导致旧数据静默失真：

- **改探针记录什么**（`Observe` 的输出）→ 必须递增该探针的 `probe_version`
- **改归一化机理**（number/letter/word 的提取算法）→ 递增对应版本
- **改 rawData 格式** → 递增 `internal/rawdata.FormatVersion`，并在 [CHANGELOG.md](./CHANGELOG.md) 写迁移说明
- 新增探针前，先在 issue 里对齐设计：观测什么、采样参数、每 cell 最小样本量、`Observation` schema、已知局限

任何"改了但没递增版本"的观测语义变更会被拒绝合并。

## 新增协议适配器

适配器只做接线（请求渲染 + 响应归一），厂商思考字段名一律经 `body_overrides` 配置提供，不写死在代码里。参考 `internal/adapter/` 现有三个实现，配套 `Render` / `Normalize` 的表驱动测试。

## 提交规范

- 格式：`<type>: <摘要>`，type 取 feat / fix / docs / test / refactor / chore
- 一次提交做一件事

## 发布流程（维护者）

- 代码发布：main 上打 `v*` tag，GitHub Actions 自动 goreleaser 出 draft release，人工核对后 Publish
- 官方基线发布：见 [docs/BASELINES.md](./docs/BASELINES.md)

## 报告问题

提 issue 请附：配置文件（抹掉密钥环境变量值）、两侧 rawData 的 manifest 段、`tv` 的完整输出与退出码。寻找模型冒名/偷换行为的复现案例尤其欢迎——请用自建题库（公开题库对被测方可见，复现才可靠）。
