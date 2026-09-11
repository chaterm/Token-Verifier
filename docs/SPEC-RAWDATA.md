# rawData 格式规范

> rawData 是这个工具的中心工件。它是采集阶段的唯一产物、比较阶段的唯一输入，
> 也是官方基线的发布形式。

## 1. 文件形态

```
<name>.rawdata.jsonl.gz
```

gzip 压缩的 NDJSON。三段式，每行一个 JSON 对象，用 `kind` 区分：

```
第 1 行      {"kind": "manifest",   ...}
第 2..N 行   {"kind": "record",     ...}
最后一行     {"kind": "aggregates", ...}
```

选这个形态的理由：

| 需求 | 如何满足 |
| :--- | :--- |
| 便于 release 分发 | 单文件，无目录结构 |
| 体积 | gzip 对高度重复的 JSON 压缩比很好 |
| 可检查 | 解压后可直接 `grep` / `jq` 逐行处理 |
| 断点续跑 | 追加写，**文件本身即续跑状态**，无需额外检查点 |
| 流式读取 | 比较阶段无需把整个文件读进内存 |

### 续跑语义

重启时读回已有 record，收集已完成的
`(probe_id, question_id, context_bucket, protocol, transport_mode, repeat_index)`
组合并跳过。`attempt` 不参与续跑键 —— 一个组合只要有任一 attempt 成功即视为完成。

若续跑时配置已变（`plan_digest` 与文件里的 manifest 不一致），拒绝续写并报错。
否则会得到一个混合了两种采集计划的文件，而它的 manifest 只描述其中一种。

### aggregates 的位置

`aggregates` 写在最后，因为它需要全部 record 才能算出。文件在采集完成前
没有这一行 —— 这正好是「采集未完成」的标志。比较阶段遇到缺 `aggregates`
的文件时：可以继续（自行遍历 record 重算），但报告中标记该侧数据不完整。

## 2. manifest

```json
{
  "kind": "manifest",
  "format_version": 1,
  "tool_version": "0.2.0",
  "created_at": "2026-09-04T12:00:00Z",

  "collection_plan": { ... },
  "plan_digest": "sha256:9f2c1a…",
  "probe_digests": {
    "onetoken": "sha256:3d7e…",
    "tokenizer": "sha256:b81c…"
  },

  "endpoint": {
    "model": "example-model",
    "api_key_fingerprint": "a3f91b2c",
    "base_url_fingerprint": "9f2c1ab3d4e5f607"
  },

  "capabilities": {
    "openai-chat": {
      "stream": true,
      "tools": true,
      "thinking": true,
      "context_window": 131072
    },
    "anthropic-messages": {
      "stream": true,
      "tools": true,
      "thinking": false,
      "context_window": 200000
    }
  },

  "skipped": [
    {
      "probe_id": "think-effort",
      "protocol": "anthropic-messages",
      "reason": "capability_missing",
      "detail": "thinking not accepted on this protocol"
    }
  ],

  "sampling": {
    "onetoken": {
      "openai-chat/stream": 7,
      "openai-chat/non_stream": 7,
      "anthropic-messages/stream": 6,
      "anthropic-messages/non_stream": 0
    },
    "tokenizer": {
      "openai-chat/stream": 1,
      "openai-chat/non_stream": 0,
      "anthropic-messages/stream": 0,
      "anthropic-messages/non_stream": 0
    }
  },

  "raw_level": "digest"
}
```

| 字段 | 说明 |
| :--- | :--- |
| `format_version` | rawData 格式版本。不同版本间不可比 |
| `tool_version` | 采集工具版本。**不参与 digest** |
| `collection_plan` | 见 §3。digest 的计算输入 |
| `plan_digest` | 全局 digest，比较阶段的硬闸 |
| `probe_digests` | 逐探针 digest，用于把冲突定位到探针 |
| `endpoint.api_key_fingerprint` | 密钥 SHA-256 的前 8 个十六进制字符。**不含明文** |
| `endpoint.base_url_fingerprint` | base_url 主机名 SHA-256 的前 16 个十六进制字符。只用于识别是否换过端点，**不含明文主机名**（rawData 可公开，不暴露厂商/网关身份）。不参与 digest |
| `capabilities` | 配置中逐协议声明的能力，原样记录。**不参与 digest**（网关接线细节；裁剪效果体现在 `sampling` / `skipped`，两侧对不上只出 NOTE） |
| `skipped` | 被跳过的 (探针, 协议) 组合及原因。**必须显式记录** |
| `sampling` | 逐探针的 (protocol, transport) 整数分配表。**记录但不进 digest**，见下 |

`skipped` 不可省略。若一侧的某探针因缺能力被跳过而不留痕迹，比较时会表现为
「该探针只有一侧有数据」，成因不明。记录下来，报告才能直接说明是能力差异
而非采集失败。

## 3. collection_plan

**这是 digest 的计算输入。** 只包含影响「采到什么样的数据」的字段。

```json
{
  "suite_version": "v2",
  "question_set_digest": "sha256:7ac2…",
  "padding_algo_version": "padding/v2",
  "padding_seed": 12345,
  "normalize": {
    "maps": { "color": { "red": ["red", "红", "rouge"] } },
    "digits": {},
    "letters": {}
  },
  "output_contract": {
    "field": "answer",
    "system_prompt": "请只回答一个合法的 JSON 对象，格式为 {\"answer\": \"<your answer>\"}，…"
  },
  "probes": {
    "onetoken": {
      "probe_version": "2",
      "repeats": 20,
      "temperature": 1.0,
      "top_p": null,
      "max_tokens": 16,
      "context_buckets": [0, 32000],
      "thinking_effort": null,
      "normalize_rule": "number",
      "observation_schema": "onetoken/v2",
      "cell_key": ["question_id", "context_bucket"],
      "question_ids": ["ot.v1.001", "ot.v1.002", "…"],
      "min_n": 10
    },
    "tokenizer": {
      "probe_version": "1",
      "repeats": 1,
      "temperature": 0.0,
      "max_tokens": 16,
      "context_buckets": [0],
      "thinking_effort": null,
      "normalize_rule": "none",
      "observation_schema": "tokenizer/v1",
      "cell_key": ["question_id", "context_bucket", "protocol"],
      "question_ids": ["tk.v1.001", "…"],
      "min_n": 1
    }
  }
}
```

协议的 `capabilities` / `thinking` 与 `request` 注入配置**不在 collection_plan 里**：
它们是网关接线细节（同一实验在不同网关上的拼写），不属于可比性契约。
接线信息记录在 manifest 的 `capabilities` / `sampling` / `skipped` 段，
比较时两侧对不上只出 NOTE，不拒绝。

### 为什么这些字段进 digest

| 字段 | 若两侧不同会怎样 |
| :--- | :--- |
| `suite_version` / `question_set_digest` | 题目不同，分布无从比较 |
| `padding_algo_version` / `padding_seed` | 长上下文填充文本的生成方式或种子不同，实际输入不同 |
| `probes.<id>.min_n` | 比较侧判 insufficient 的门槛不同，参与统计的 cell 集合不同 |
| `output_contract` | 对模型的格式约束不同（system prompt 强约束，见 SPEC-SUITE §4.1），答案分布不可比；纯 raw 题库为 null |
| `normalize`（词表/扩展表） | 同一原文归一化出不同观测值。词表由题库提供（见 SPEC-SUITE §4），进 digest 后改词表无需人工递增版本即被闸门拦截 |
| `probe_version` | 观测语义变了，同名字段含义不同 |
| `repeats` | 样本量不同，统计功效不可比 |
| `temperature` / `top_p` | 采样分布本身就不同 |
| `max_tokens` | 可能导致截断，改变答案分布 |
| `context_buckets` | 上下文长度不同，探针压力不同 |
| `thinking_effort` | 思考强度不同，行为不同 |
| `normalize_rule` | 归一化不同，同一原文得到不同观测值 |
| `observation_schema` | 观测结构不同，字段无法对齐 |
| `cell_key` | 分组粒度不同，配对方式不同 |
| `question_ids` | 题目子集不同 |


**协议与传输的分配表（`sampling`）不在其中。** 比例是使用者自己的测试杠杆：
想验证「不同请求格式下模型行为是否一致」，就按自己选的比例混合；不想验证，
就用单一格式。把它锁进 digest 会剥夺这个杠杆 —— 官方基线用单一格式发布时，
任何想混格式采集的使用者都会立刻变得不可比。

这里有一个明示的前提：**请求格式（协议封装、流式与否）不改变背后模型的指纹。**
该前提若被违反，表现形态是自洽的：混格式采集与官方指纹不符、而单格式采集相符，
使用者由此自然把问题定位到「格式相关」的行为上。比较阶段会对两侧 `sampling`
的差异输出提示性 NOTE（不是拒绝），帮助使用者做这个归因。

### 明确不进 digest

| 字段 | 为什么 |
| :--- | :--- |
| `base_url` / `api_key` | 换端点、换密钥是**要检测的对象**，不是可比性前提（manifest 只落指纹） |
| `protocols[].capabilities` / `thinking` / `path` / `weight` / `transport` | 网关接线与测试杠杆：同一实验在不同网关上的拼写不同，不该被闸门拒绝。题目「用哪个思考档位」随题库（`thinking_effort`）进 digest；`effort_map` 只是该档位的翻译 |
| `request.temperature_field` / `body_overrides` | 温度值放哪个点路径、注入了哪些厂商特有字段，都是接线路由；温度的**值**已随 `probes.<id>.temperature` 进 digest。**约束**：`body_overrides` 只用于网关接线兼容，不用于语义采样参数（`top_k`、`seed` 之类请用/等待专用字段），否则两侧差异会逃过闸门 |
| `max_concurrency` / `timeout` / `max_attempts` | 影响失败率，不影响样本内容 |
| `min_interval_ms` | 同上 |
| `seed` | 只决定请求时间顺序，不决定各档样本数 |
| `raw_level` | 脱敏程度，不影响观测值 |
| `tool_version` / `created_at` | 元信息 |
| **阈值** | 判据不是采集参数。见下 |

**阈值不进 digest 是让两条规则不冲突的关键**：使用者可以自由调整阈值而不影响
与官方 rawData 的可比性；而任何采集参数的改动都会被闸门拦下。若阈值进了
digest，「阈值用户可设」与「计划差异一律拒绝」就会互相矛盾。

### digest 计算

```
plan_digest = "sha256:" + hex(SHA256(canonical_json(collection_plan)))
```

canonical JSON 规则：

1. 对象键按字节序升序排列（递归）
2. 无任何多余空白（无缩进、无键值间空格）
3. 数字规范化：整数不带小数点与正号；浮点用最短往返表示；不用指数记法
4. 字符串统一 UTF-8，转义使用最短合法形式
5. `null` 字段**保留**，不省略 —— `"top_p": null` 与缺失该键必须产生相同结果，
   实现上统一保留显式 `null`
6. 数组顺序有意义，不排序（`question_ids` 与 `context_buckets` 已在生成时排序）

`probe_digest` 对 `collection_plan.probes[<id>]` 单独计算 —— 改一个探针的采集
参数只会让 `plan_digest` 变化，probe_digest 帮助定位到受影响的探针。
协议的 `thinking` 块不折入（网关接线细节，见上）。

## 4. record

每次请求尝试一条。

```json
{
  "kind": "record",
  "probe_id": "onetoken",
  "question_id": "ot.v1.003",
  "context_bucket": 0,
  "protocol": "openai-chat",
  "transport_mode": "stream",
  "thinking_effort": null,
  "repeat_index": 5,
  "attempt": 0,

  "status": "success",
  "http_code": 200,
  "error_kind": null,
  "error_detail": null,
  "latency_ms": 842,
  "ttft_ms": 311,
  "tps": 47.2,
  "usage": {
    "prompt": 128,
    "completion": 3,
    "reasoning": 0,
    "cached": 0
  },

  "observation": { "value": "7" },

  "raw_content_sha256": "sha256:e3b0c4…"
}
```

### 定位字段

| 字段 | 类型 | 说明 |
| :--- | :--- | :--- |
| `probe_id` | string | |
| `question_id` | string | 题库内唯一 |
| `context_bucket` | int | `0` 表示不加填充 |
| `protocol` | string | 实际使用的协议 |
| `transport_mode` | string | `stream` \| `non_stream` |
| `thinking_effort` | string \| null | 符号级别，非映射后的值 |
| `repeat_index` | int | 该 cell 内的序号，从 0 开始 |
| `attempt` | int | 该次的尝试序号，从 0 开始 |

`thinking_effort` 存符号级别而非映射后的具体值。具体值可从 manifest 的
`effort_map` 推出，而符号级别才是跨协议可比的那一层。

### 传输字段

| 字段 | 类型 | 说明 |
| :--- | :--- | :--- |
| `status` | string | `success` \| `error` |
| `http_code` | int \| null | 未收到响应时为 null |
| `error_kind` | string \| null | `timeout` \| `connection` \| `http_4xx` \| `http_5xx` \| `protocol` \| `parse` |
| `error_detail` | string \| null | 简短错误摘要，不含密钥 |
| `latency_ms` | int | 整体墙钟 |
| `ttft_ms` | int \| **null** | **非流式时为 null**，不填 0 |
| `tps` | float \| null | 非流式时为 null |
| `usage` | object | 归一后的 token 计数，各协议差异已在 Adapter 内消除 |

`ttft_ms` 在非流式下必须是 `null`。填 0 会让下游无法区分「未测量」与「测得为 0」，
而这类占位符一旦透传出去，字段就永久失去意义。

`error_detail` 需过滤：端点回显的错误信息里可能包含请求体片段，而请求头里有
密钥。写入前做一次密钥值的字面替换。

### 观测字段

`observation` 的结构由探针定义，`observation_schema` 标注其版本。

采集时已完成归一化与匹配 —— 这是硬性要求，因为 `digest` 档不保留原文，
所有统计必须仅凭 `observation` 可算。

| 探针 | observation 形状 | 说明 |
| :--- | :--- | :--- |
| `onetoken` | `{"value": "7"}` | 已归一化的答案 |
| `tokenizer` | `{"prompt_tokens": 128}` | 服务端上报值 |
| `needle` | `{"matched": [true, false]}` | **布尔数组**，按埋点位置序号对齐；一个埋点一个布尔 |
| `think-effort` | `{"reasoning_value": 512, "unit": "tokens"}` | 单位由协议静态决定：`tokens` = usage 上报的思考 token 数（openai 两协议）；`chars` = 交付的思考文本 rune 数（anthropic-messages，usage 无独立思考字段）。tokens 档缺失/为零记观测错误；chars 档 0 是有效观测（思考文本被清零本身是降级信号）。cell_key 含 protocol，跨单位不会进同一 cell |
| `toolcall` | `{"tool": "get_weather", "args_valid": true, "parallel": false, "num_calls": 1}` | `tool` 为首个调用的工具名；`args_valid` = 工具名在生效工具集内且参数是合法 JSON object（**语法合法**，不做 schema 字段级校验）；`parallel` = `num_calls > 1` |

`needle` 的 `matched` 是布尔数组，这一点是刻意的。参考实现在这里把匹配结果塞进
一个名叫 `p_value` 的字段，用 `0.0` 表示匹配、`1.0` 表示不匹配，而同一份代码
的另一条路径用了相反约定。这种「名字说是 p 值、实际是反转布尔、且两处约定
相反」的字段几乎注定被误用。此处统一为布尔语义；题目声明多个埋点位置时，
数组下标即位置序号（一个埋点 = 比较侧一个统计单元），保留「哪个深度开始丢」
的定位信息。

### 原文字段

| 字段 | `raw_level=digest` | `raw_level=full` |
| :--- | :--- | :--- |
| `raw_content_sha256` | 有 | 有 |
| `raw_content` | 无 | 有 |
| `raw_reasoning` | 无 | 有（若存在） |
| `raw_chunks` | 无 | 有（流式时，含每 chunk 的相对时间戳） |

`digest` 档保留原文哈希：两份 rawData 的同一个 cell 若哈希完全一致，可以判断
是否命中了缓存或是否为同一份响应的复制，而不需要暴露原文。

## 5. aggregates

```json
{
  "kind": "aggregates",
  "record_count": 1420,
  "cells": {
    "onetoken|ot.v1.003|0": {
      "n": 20,
      "n_valid": 20,
      "distribution": { "7": 9, "3": 6, "5": 5 }
    },
    "tokenizer|tk.v1.001|0|openai-chat": {
      "n": 1,
      "n_valid": 1,
      "values": [128]
    },
    "think-effort|te.v1.002|0|high|openai-chat": {
      "n": 10,
      "n_valid": 10,
      "values": [812, 790, 843, 805, 798, 820, 831, 776, 809, 815]
    },
    "toolcall|tc.v1.001|0": {
      "n": 10,
      "n_valid": 10,
      "distribution": { "get_weather": 8, "get_forecast": 2 }
    }
  },
  "transport": {
    "overall": {
      "availability": 0.998,
      "error_rate": 0.002,
      "timeout_rate": 0.001,
      "latency_ms": { "p50": 780, "p90": 1240, "p99": 2100 },
      "ttft_ms": { "p50": 290, "p90": 470, "p99": 810 },
      "tps": { "p50": 45.1, "p90": 58.3 }
    },
    "by_protocol": { "openai-chat": { "…": "…" } },
    "by_transport_mode": { "stream": { "…": "…" } },
    "by_protocol_transport": { "openai-chat/stream": { "…": "…" } }
  }
}
```

`cells` 的键是 cell key 各分量以 `|` 连接（tokenizer 含 protocol、think-effort
含 thinking_effort 与 protocol，与采集计划的 `cell_key` 一致）。`n` 是记录数，
`n_valid` 是 `status=success` 的数。

`aggregates` 是**派生数据**，可从 record 完整重算。存它只为让比较阶段无需
全量遍历。若与 record 不一致，以 record 为准。

### 传输指标的分层

四种切片同时提供。`by_protocol_transport` 是最细的一层，也是排查「某协议的
流式特别慢」这类问题时唯一有用的一层。

**这些指标不参与 pass/fail 判定**，除非使用者显式为它们提供阈值。理由见
[DATAFLOW.md](./DATAFLOW.md) §3.5。

## 6. 比较阶段的读取要求

1. 读第 1 行，取 `plan_digest`。两侧不等 → 逐字段 diff，`exit 3`
2. 检查 `format_version` 一致
3. 流式读 record，按各探针的 `cell_key` 分组
4. 只保留 `status=success` 的 record 参与统计
5. 两侧都存在的 cell 才配对
6. 任一侧 `n_valid` 低于该探针要求的下限 → 整个 cell 标 `insufficient` 并剔除

第 6 条的下限由探针自行声明。分布距离类探针对样本量敏感 —— 用少量样本估出的
分布形状不足以支撑判定，这类 cell 必须剔除而非降权。

## 7. 脱敏与发布

官方 release 用 `raw_level=digest`。该档下文件内容为：

- CollectionPlan（不含 `base_url`、不含密钥）
- 密钥指纹（SHA-256 前 8 位）与主机名指纹（SHA-256 前 16 位）
- 逐协议的能力声明（`capabilities`，不含明文主机名）
- 每条 record 的定位、传输指标、`observation`、原文哈希

不含：原始 prompt 之外的任何模型输出文本、思考内容、密钥、完整 URL、明文主机名。

**长上下文埋点标记的处理**：标记由 (题目 ID, 位置序号) 经哈希确定性派生，
不以明文出现在 rawData 中，也不出现在题库文件里（题库只声明相对位置）。
`observation` 只有 `matched` 布尔数组。这样公开 rawData 不会泄露埋点内容，
被测端点也无法通过读取公开数据预知标记。

## 8. 版本演进

| 变更 | 后果 |
| :--- | :--- |
| 新增 record 可选字段 | `format_version` 不变，旧读取器忽略新字段 |
| 修改现有字段语义 | 必须递增 `format_version`，跨版本不可比 |
| 修改探针观测语义 | 必须递增该探针 `probe_version` → digest 变化 → 旧 rawData 自动不可比 |
| 修改归一化规则 | 同上 |
| 新增探针 | 无影响；未启用该探针的旧 rawData 在比较时该项缺失，报为不可比 |

`probe_version` 是防止语义漂移的主要机制。任何改变「同一份响应会得到什么
observation」的修改都必须递增它 —— 否则旧数据会与新数据静默比较，得出错误结论。
