# Changelog

所有显著变更记录于此。观测语义变更（影响 rawData 可比性的）用 **粗体** 标注。

格式基于 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

## [Unreleased]

### Added

- GitHub Actions CI：gofmt / go vet / go test 三平台矩阵 + golangci-lint
- goreleaser 发布配置：多平台二进制内置示例配置与题库，`v*` tag 自动出 draft release
- `Publish Baseline` workflow：题库 + 官方 rawData 成对发布（强制 digest 级门禁）
- 社区文件：CONTRIBUTING / CODE_OF_CONDUCT / SECURITY
- docs/BASELINES.md 官方基线索引

### Changed

- **`ToolVersion` 从常量 `0.2.0` 改为由 ldflags 注入（源码构建时为 `dev`）**：发布版 rawData manifest 的 tool_version 将与 git tag 一致
- 示例配置改为模型无关占位；示例题库精简为 `suites/v1.example.yaml`
