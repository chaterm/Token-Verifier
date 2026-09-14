# 官方基线

官方基线 = **题库快照 + config 存档 + digest 级 rawData**，成对发布于独立的数据仓库
**[`chaterm/Token-Verifier-Data`](https://github.com/chaterm/Token-Verifier-Data)** 的 GitHub
Release（tag 形如 `baseline-<model>-suitevN`）。基线是**不可变发布物**，不进 git 版本历史；
下载与校验以 Release 附件的 `SHA256SUMS` 为准。

**基线索引表、下载方式、维护者发布流程都在数据仓库的 README**：
[chaterm/Token-Verifier-Data](https://github.com/chaterm/Token-Verifier-Data#基线索引)。
本页只保留与工具语义相关的版本兼容说明与已知限制。

使用任一基线前请先读 README 的「使用官方基线」一节：你的配置必须与该基线的采集计划一致才能通过 digest 闸门，具体以 rawData 第 1 行 manifest 的 `collection_plan` 字段为准。

## 版本兼容矩阵

rawData 的可比性由四层版本共同决定，任一不一致 digest 闸门都会拒绝：

| 版本层 | 载体 | 何时变化 | 不一致时 |
|---|---|---|---|
| tool_version | manifest | 每次发版（git tag） | 仅提示，不阻断（不进 digest） |
| format_version | manifest | rawData 格式改动 | 拒绝比较 |
| probe_version | 每探针 manifest | 探针观测语义改动 | 该探针拒绝比较 |
| suite_version / question_set_digest | collection_plan | 改题文本 / 增删题 | 拒绝比较 |

发布新版本工具后，旧基线**不需要**立即重采：只要 probe_version 与 format_version 没变，`tv compare` 仍可比。出现兼容断裂时，在数据仓库的索引表对应基线标注「已过时」，但**不删除** Release——历史可复现优先。

## 维护者：发布新基线

发布流程由数据仓库的 `Publish Baseline` workflow 承担（draft release 门禁：校验 digest 级、题库 `suite_version` 与 manifest 一致，自动改写 config 的 `suite.path` 并填入 `suite.sha256`，发布后自动更新索引表）。步骤见 [Token-Verifier-Data README「维护者：发布新基线」](https://github.com/chaterm/Token-Verifier-Data#维护者发布新基线)。

## 已知限制

- **公开题库可被针对性优化**：官方题库公开后，被测厂商理论上可以据此调教表现。官方基线的价值是全社区用同一把尺子（可比性），不是防作弊——对抗性验证请用自建题库。
- **基线有时效性**：模型会被厂商静默更新，半年前的基线未必仍代表「该模型官方行为」。采集窗口列即为此存在；对时效敏感的结论请自行重采。
