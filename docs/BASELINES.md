# 官方基线索引

官方基线 = **题库快照 + digest 级 rawData**，成对发布于 GitHub Release（tag 形如 `baseline-<model>-suitevN>`）。本页只是人读索引；下载与校验以 Release 附件的 `SHA256SUMS` 为准。

使用任一基线前请先读 README 的「使用官方基线」一节：你的配置必须与该基线的采集计划一致才能通过 digest 闸门，具体以 rawData 第 1 行 manifest 的 `collection_plan` 字段为准。

## 基线列表

| 模型 | suite | tool 版本 | 采集窗口 | Release | 状态 |
|---|---|---|---|---|---|
| （暂无——首条基线发布后填入） | | | | | |

> 发一条基线就在这里加一行，并同步更新下方的版本兼容矩阵。

## 版本兼容矩阵

rawData 的可比性由四层版本共同决定，任一不一致 digest 闸门都会拒绝：

| 版本层 | 载体 | 何时变化 | 不一致时 |
|---|---|---|---|
| tool_version | manifest | 每次发版（git tag） | 仅提示，不阻断（不进 digest） |
| format_version | manifest | rawData 格式改动 | 拒绝比较 |
| probe_version | 每探针 manifest | 探针观测语义改动 | 该探针拒绝比较 |
| suite_version / question_set_digest | collection_plan | 改题文本 / 增删题 | 拒绝比较 |

发布新版本工具后，旧基线**不需要**立即重采：只要 probe_version 与 format_version 没变，`tv compare` 仍可比。出现兼容断裂时，本页对应基线标注「已过时」，但**不删除** Release——历史可复现优先。

## 维护者：发布新基线

1. 在官方端点用当前官方题库采集（配置存档，勿发密钥）：

   ```bash
   tv collect -c baseline-<model>.yaml -o <model>.rawdata.jsonl.gz
   ```

2. GitHub 仓库 → Actions → **Publish Baseline** → Run workflow，上传题库 yaml 与 rawData 文件，填模型名与 tag（`baseline-<model>-suitevN`）
3. workflow 自动校验 digest 级别并创建 Release（含题库快照、rawData、SHA256SUMS）
4. 在本页表格加一行，附采集窗口

## 已知限制

- **公开题库可被针对性优化**：官方题库公开后，被测厂商理论上可以据此调教表现。官方基线的价值是全社区用同一把尺子（可比性），不是防作弊——对抗性验证请用自建题库。
- **基线有时效性**：模型会被厂商静默更新，半年前的基线未必仍代表「该模型官方行为」。采集窗口列即为此存在；对时效敏感的结论请自行重采。
