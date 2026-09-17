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
- 子集比较 `--allow-subset`（compare / run）：`plan_digest` 不等时不再一律
  拒绝，按字段三分法调和后只比较交集 —— strict 字段（temperature / top_p /
  max_tokens / thinking_effort / probe_version / observation_schema /
  normalize_rule / cell_key / question_ids 与整个 plan 层）仍须一致；
  `context_buckets` 取交集、单侧独有档位剔除并列出；`repeats` 取小且 cell 内
  按 `repeat_index` 降采样到 `min(nA,nB)`；`min_n` 闸门忽略，比较时按
  `本地 config > 计划烘焙值 > 缺省` 解析（判据参数，与 thresholds 同类）。
  `question_set_digest` 不参与 strict 比对 —— 它由启用探针的题目并集派生，
  两侧启用探针集合不同（如一侧禁用 needle）时必然不等，正是子集模式要容忍的
  差异；真实题库差异由逐探针 `question_ids` 的 strict 比对拦截，该值不等时
  输出 NOTE 说明。报告强制披露范围：stdout 顶部 `SCOPE` 段、JSON `subset`/`scope`、
  JUnit properties；全 pass 退出码为新增的 `6`（不是 `0`），fail/inconclusive
  优先级不变。digest 计算不变，已有 rawData 与官方基线继续有效。
  默认严格模式行为与旧版完全一致（DATAFLOW §3.2）

### Fixed

- 适配器容忍网关附加的空字符串 `error` 字段：AWS Bedrock 在成功响应上
  附加 `"error": ""`（字符串形态），此前三个协议（anthropic-messages /
  openai-chat / openai-responses）的非流式与流式解析都只认对象形态
  `{"message":...}`，整包 JSON 解析失败 —— HTTP 200、内容正常的响应被
  误判为 `status=error, error_kind=parse`。现在空串/null 视为无错误；
  非空字符串按错误文案上报 protocol 错误；标准对象形态行为不变
- tokenizer 探针的 cell 判等从「多重集相等（先比条数）」改为「取值集合相等」：
  两侧条数不同（repeats 配置不同、或个别请求重试耗尽失败少一条 record）但
  服务端上报的 `prompt_tokens` 取值一致时，旧实现判 MISMATCH，可能产生假
  fail。分词器一致性只取决于取值集合，与条数无关
- 子集模式下阈值校验只要求交集档位的阈值：此前无探针级兜底时，A 侧声明而
  B 侧没有的档位也会因缺 `<probe>.<bucket>` 阈值报错（按 A 侧全量档位校验）
- compare 侧计划镜像结构补上遗漏的 `top_p` 字段（不影响 digest 与逐字段
  diff —— 它们走原始 JSON —— 但字段级调和需要它）

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
- 判定语义变更（不影响 rawData 可比性）：tokenizer 的 cell 判等改为集合语义
  （见 Fixed）。两侧条数不等但取值一致的 cell 从 MISMATCH 变为 match，
  此前依赖旧行为「碰巧」fail 的结论可能翻转为 pass
- 判定语义变更（不影响 rawData 可比性）：`min_n` 按判据参数处理（与
  thresholds 同类），比较时按 `本地 config > 计划烘焙值 > 缺省` 解析，
  严格模式同样生效 —— 采集后修改本地 config 的 `probes.<id>.min_n` 再带
  `-c` 比较，同 digest 数据的判定会随之变化（此前只认计划烘焙值）。
  不带 `-c` 时行为与旧版一致
- openai-chat 流式请求默认携带 `stream_options: {include_usage: true}`
  （Chat Completions 流式不显式开启时服务端不上报 usage，观测会缺 token 数）。
  非保留字段，端点不识别时可经 `body_overrides` 覆盖
- **`ToolVersion` 从常量 `0.2.0` 改为由 ldflags 注入（源码构建时为 `dev`）**：发布版 rawData manifest 的 tool_version 将与 git tag 一致
- 示例配置改为模型无关占位；示例题库精简为 `suites/v1.example.yaml`

### Security

恶意端点威胁模型下的防护（`base_url` 指向的主机本身可能是攻击者，
均经真实 PoC 复验，详见 SECURITY.md）：

- **不跟随 HTTP 重定向**：此前 `http.Client` 用 Go 默认重定向策略，恶意
  端点返回 3xx 即可把认证头转发到任意第三方主机 —— Go 默认只剥离
  `Authorization` 等标准头，anthropic 协议的 `x-api-key` 被原样转发
  （实测确认），且请求被记为成功。现在 3xx 作为最终响应处理，记新增
  `error_kind=http_3xx`（不重试，与 4xx 同类），攻击者主机零访问
- **响应体大小封顶 512KiB**：非流式 `readAll` 与三个协议的流式内容累积
  （content / reasoning / 工具参数分片）共用同一预算，超限记 `parse`
  错误。此前 `io.ReadAll` 无上限，恶意端点用 gzip 炸弹（HTTP 透明解压后
  无限流）实测 30 秒把采集进程内存推到 ~10GB
- **`error_detail` 落盘前按 4KiB 截断**（rune 边界）：恶意端点可在
  HTTP 200 响应体内嵌超大 `error.message`，此前无界字符串直达 rawData
  JSONL 单行；超过读回侧 `bufio.Scanner` 的 16MB 单行上限会让证据文件
  永久不可读（当次采集收尾即失败）
- **verbose 报告剥控制字符**：onetoken 答案取值 / toolcall 工具名源自
  服务端文本，渲染进对齐表格前过滤 C0 控制字符（含 ESC）与 DEL，
  防 ANSI 转义序列伪造终端显示
