# 配置规范

> 本文定义配置文件的完整 schema、比例分配算法与校验规则。
>
> **关于示例中的厂商字段名**：工具本身不认识任何厂商的私有字段。所有厂商特有的
> 请求字段都由使用者通过 `body_overrides` 原样提供，工具只负责把它合并进请求体
> 并展开占位符。本文示例中出现的字段名仅为**结构示意**，实际字段名与取值范围
> 请以你所对接端点的当时文档为准 —— 这也正是把它做成配置而非内置映射的原因：
> 厂商字段是每季度都在变的移动目标，写进代码就会过期。

## 1. 顶层结构

```yaml
version: 1                    # 配置格式版本

target:                       # 待测端点，collect / run 需要；compare 不需要
  base_url: ...
  api_key_env: ...
  model: ...
  protocols: [...]
  request: {...}

suite:                        # 题库，一律由外部文件提供，工具不内置
  path: ./suite.yaml          # 必填，格式见 SPEC-SUITE.md
  sha256: ...                 # 可选；给了则必须校验通过

probes:                       # 启用哪些探针及其采集参数
  onetoken: {...}
  tokenizer: {...}
  needle: {...}
  think-effort: {...}
  toolcall: {...}

thresholds:                   # 比较判据，compare / run 必填；工具不提供默认值
  onetoken: 0.15              # <probe>.<bucket> 可做档位级覆盖
  tokenizer: 0.01
  needle: 0.01

runtime:                      # 不进 digest 的运行时参数
  max_concurrency: 4
  timeout_sec: 120
  min_interval_ms: 0
  max_attempts: 3
  seed: 20260904
  raw_level: digest
  backoff_base_ms: 500
  backoff_max_ms: 30000

padding:                      # 长上下文填充（bucket > 0 时生效）
  seed: 12345                 # 可选；缺省 12345（padding/v1 时代的固定值）
```

`runtime` 不进 digest。`suite`、`probes`（含 `min_n`）与 `padding.seed` 进 digest；
**整个 `target` 段不进 digest** —— 它是「往哪个网关、用什么拼写把实验发出去」的
接线细节，同一套实验在不同网关上的写法不该被闸门拒绝。其中：
- `base_url` / `api_key_env` / `model` 是端点身份（manifest 只落密钥与主机名指纹）；
- 各协议的 `capabilities` / `thinking` / `path` / `weight` / `transport` 是能力声明与
  测试杠杆，裁剪效果记录在 manifest 的 `sampling` / `skipped` 段（见 SPEC-RAWDATA.md §2），
  比较时两侧对不上只出 NOTE，不拒绝；
- `request.temperature_field` / `body_overrides` 是字段注入路由，温度的**值**已随
  `probes.<id>.temperature` 进 digest。

`thresholds` 不进 digest（它是判据，不是采集参数）。

> **digest 只锁实验设计，不锁网关接线。** 题目「用哪个思考档位」由题目级
> `thinking_effort` 承载、随题库进 digest；协议的 `effort_map` 只是把该档位翻译成
> 具体网关的字段写法（字符串级别或整数预算），属于接线，不进 digest。
> 一条使用约束：`body_overrides` 只用于网关接线兼容（厂商特有字段），**不要**用它塞
> 语义采样参数（如 `top_k`、`seed`）—— 那类参数应走专用字段（进 digest），否则两侧
> 差异会逃过闸门。

**可比性闸门默认要求 digest 逐字节一致**；`tv compare --allow-subset`（`tv run`
同名）把 `probes` 段放宽为字段级三分法：采样/身份字段（`temperature` / `top_p` /
`max_tokens` / `thinking_effort` 及探针版本、观测 schema、归一化规则、`cell_key`、
`question_ids`）仍须一致；`context_buckets` 取交集；`repeats` 取小并按
`repeat_index` 降采样对齐；`min_n` 闸门忽略、比较时按 `本地 config > 计划烘焙值 >
缺省` 解析。完整规则见 DATAFLOW.md §3.2。

## 2. target

```yaml
target:
  base_url: https://api.example.com
  api_key_env: TARGET_API_KEY      # 只读环境变量名，密钥不写入配置文件
  model: example-model

  protocols:
    - id: openai-chat
      weight: 0.6
      path: /v1/chat/completions    # 可选，覆盖该协议默认路径
      capabilities:                 # 声明式，工具信任它，不做前置探测
        stream: true
        tools: true
        thinking: true
        context_window: 131072
      transport:
        stream: 0.5
        non_stream: 0.5
      thinking:
        disabled:
          body_overrides: {}
        enabled:
          effort_map:
            low:    low
            medium: medium
            high:   high
          body_overrides:
            reasoning_effort: "${thinking_effort}"

    - id: anthropic-messages
      weight: 0.4
      capabilities:
        stream: true
        tools: true
        thinking: false
        context_window: 200000
      transport:
        stream: 1.0
        non_stream: 0.0
      thinking:
        disabled:
          body_overrides: {}
        enabled:
          effort_map:
            low:    1024
            medium: 4096
            high:   16384
          body_overrides:
            thinking:
              type: enabled
              budget_tokens: "${thinking_effort}"

  request:
    temperature_field: temperature   # 点路径，见 §4
    body_overrides: {}               # 所有协议共享的追加字段
```

| 字段 | 必填 | 说明 |
| :--- | :--- | :--- |
| `base_url` | 是 | 端点基址。是否需要包含版本前缀取决于端点，路径拼接规则见 `path` |
| `api_key_env` | 是 | 环境变量名。**不接受**直接写密钥的字段 |
| `model` | 是 | 模型标识，作为请求体的 `model` 字段 |
| `protocols` | 是 | 至少一项。见 §3 |
| `request.temperature_field` | 否 | 默认 `temperature` |
| `request.body_overrides` | 否 | 与协议级 `body_overrides` 合并，协议级优先 |

密钥只以环境变量名的形式出现在配置里。采集时读取其值用于请求，但只把该值的
SHA-256 前 8 位写进 rawData（用于识别「是否换过 key」），明文永不落盘。

## 3. protocols

### 3.1 协议标识

| `id` | 目标形态 |
| :--- | :--- |
| `openai-chat` | Chat Completions 风格 |
| `openai-responses` | Responses 风格 |
| `anthropic-messages` | Messages 风格 |

`id` 决定使用哪个 Adapter，即消息如何封装、响应如何解析、SSE 事件如何切分。
新增协议 = 新增一个 Adapter 实现 + 在此处可选。

### 3.2 capabilities：声明式，不探测

```yaml
capabilities:
  stream: true          # 该协议上流式是否可用
  tools: true           # 是否支持工具调用
  thinking: true        # 是否接受思考配置
  context_window: 131072  # 实际可用上下文窗口；0 或省略表示不限制档位
```

工具**完全信任这些声明，不发任何探测请求**。理由：探测要花钱、要时间，而且
探测结果只在该时刻有效；声明错了的后果也是可见的 —— 请求会以能力相关错误失败，
受影响的组合被跳过并记录原因（manifest 的 `skipped`），不会静默得出错误结论。

`capabilities` 原样写进 manifest（逐协议），但**不参与 plan_digest**：它是网关接线
细节。能力声明的效果（哪些档位被裁、流式份额折算到哪里）体现在 manifest 的
`sampling` / `skipped` 段，比较时两侧对不上只出 NOTE，不拒绝 —— 同一套实验在不同
能力声明的网关上仍然可比。

`capabilities` 可省略，缺省为 `{stream: true, tools: true, thinking: true,
context_window: 0}`（`context_window: 0` 表示不按窗口裁档位）。

`context_window` 用于在分配阶段裁掉超窗的 `context_bucket`：某档位超过声明窗口
时，该档位的请求不生成，并在 manifest 中记录裁掉的原因。

### 3.2 weight

各协议 `weight` 的相对比例决定请求分配。**不要求和为 1** —— 会先归一化。
`weight: 0` 表示该协议配置保留但本次不采集。

### 3.3 transport

```yaml
transport:
  stream: 0.5
  non_stream: 0.5
```

同样是相对比例、同样会归一化、同样允许某项为 0。**挂在每个协议下**，因为
不同协议的流式行为不同，且某端点可能只在部分协议上支持流式。

工具**完全信任配置里的声明，不做前置探测**。若采集过程中该协议实际拒绝流式
（请求以能力相关错误失败），受影响的 (协议, 传输) 组合被跳过、原因写进
manifest 的 `skipped`，而不是静默降级为非流式 —— 静默降级会让 rawData 的
`transport_mode` 与实际不符，破坏可比性。

与运行时拒绝不同，**声明** `capabilities.stream: false` 时的流式份额在分配期
折算到非流式（原因记入 `skipped`）：这是配置作者明示的能力，不存在
「transport_mode 与实际不符」的问题，折算保住了 repeats 的样本量语义。
两种路径都不静默：前者记运行时 skip，后者记分配期 skip。

### 3.4 thinking

两个互斥的块：

```yaml
thinking:
  disabled:
    body_overrides: {...}      # 题目 thinking_effort 为 null 时使用
  enabled:
    effort_map: {...}          # 符号级别 → 该协议的具体值
    body_overrides: {...}      # 用 ${thinking_effort} 标出位置
```

选择规则：

| 题目的 `thinking_effort` | 使用 | 缺该块时 |
| :--- | :--- | :--- |
| `null` 或未设置 | `disabled.body_overrides` | 视为空对象，不追加任何字段 |
| `low` / `medium` / `high` | `enabled` 块 + `effort_map` 映射 | 该 (protocol, probe) 组合跳过并记录 |

`effort_map` 缺少题目用到的级别 → 配置校验直接报错（`exit 2`），不跳过。
缺级别是配置错误，跳过会静默减少样本量。

## 4. 占位符与字段注入

### 4.1 `${thinking_effort}` 的类型保留规则

**当某字段的值是恰好等于 `"${thinking_effort}"` 的字符串时**，整体替换为
`effort_map` 映射后的值，**并保留映射值的 JSON 类型**：

```yaml
effort_map: { high: 16384 }
body_overrides:
  thinking:
    budget_tokens: "${thinking_effort}"
```

渲染结果：

```json
{ "thinking": { "budget_tokens": 16384 } }
```

不是 `"16384"`。这条规则必须严格实现：有协议的思考预算字段是整数类型，
传字符串会被端点以 400 拒绝，而这种失败会表现为「该探针全部请求失败」，
排查成本很高。

**占位符嵌在更长的字符串里时**做文本插值，结果是字符串：

```yaml
effort_map: { high: 16384 }
body_overrides:
  extra_params: "budget=${thinking_effort}"
```

渲染为 `{"extra_params": "budget=16384"}`。

占位符可出现在嵌套任意深度的对象与数组中。同一个 `body_overrides` 里可以
出现多次，全部替换为同一个值。

### 4.2 body_overrides 合并与保留字段

合并顺序（后者覆盖前者）：

```
target.request.body_overrides  →  protocols[].thinking.*.body_overrides
```

**保留字段**：`model` 与消息体字段（各协议的消息数组，如 `messages` /
`input`；输出契约的 `system`，见 SPEC-SUITE §4.1；toolcall 的工具集 `tools`，
各协议包装形态由适配器渲染）不允许通过
`body_overrides` 设置。工具需要完全掌控这些字段才能保证探针语义，覆盖它们
会让 rawData 与 CollectionPlan 描述的内容不符。校验阶段发现即报错。

### 4.3 temperature_field 点路径

部分端点把采样参数放在嵌套对象里。`temperature_field` 接受点路径：

```yaml
request:
  temperature_field: generation_config.temperature
```

渲染时在该路径上创建必要的中间对象并写入温度值。

### 4.4 思考与采样参数的相互约束

某些端点在启用思考时会**锁定或拒绝**采样参数（例如要求温度必须为某个固定值，
或不接受 `top_p`）。这类约束因协议与厂商而异，且会随版本变化。

工具的处理方式：不内置任何约束表，也不做前置探测。采集时若某协议在「启用思考 +
当前采样参数」的组合下被端点拒绝，该组合按缺能力跳过、原因写进 manifest 的
`skipped`；端点返回的原始错误摘要一并记录，供使用者据此调整配置 —— 端点自己的
报错远比工具内置的过期规则准确。

配置校验只做一件相关的事：若某协议的 `enabled.body_overrides` 与
`request.temperature_field` 指向同一路径，报冲突。

## 5. 比例分配算法

### 5.1 语义

`repeats` 是**一个 cell 的总请求数**。比例切分它，不放大它。

```
repeats = 20，协议 0.6 / 0.4：
  openai-chat        → 12    （其下再按 transport 切分）
  anthropic-messages →  8
  合计                 20
```

### 5.2 最大余数法

两级各自独立执行同一算法：

```
输入：总数 N，权重 w[1..k]
1. W = Σw[i]
2. exact[i] = N × w[i] / W
3. base[i]  = floor(exact[i])
4. rem = N − Σbase[i]
5. 按 (exact[i] − base[i]) 降序，取前 rem 项各 +1
   —— 小数部分相同时按数组下标升序，保证结果确定
6. alloc[i] = base[i] (+1)
```

保证 `Σalloc[i] = N` 恰好成立。第 5 步的平局规则必须确定，否则同一配置在
不同运行里会产生不同分配表 —— 分配表虽不进 digest，但会写进 manifest 并
参与续跑键与报告，非确定性会让同一份配置产出两份内容不同的 rawData。

先按协议分配，再对每个协议分得的数量按其 `transport` 比例分配。

### 5.3 分配表只记录，不进 digest

分配表写入 manifest 的 `sampling` 段（见 SPEC-RAWDATA.md §2），**不参与
digest 计算**。

比例是使用者自己的测试杠杆：想验证「不同请求格式下模型行为是否一致」，就按
自己选的比例混合；不想验证，就用单一格式。把它锁进 digest 会剥夺这个杠杆 ——
官方基线用单一格式发布时，任何想混格式采集的使用者都会立刻变得不可比。

这里有一个明示的前提：**请求格式（协议封装、流式与否）不改变背后模型的指纹。**
该前提若被违反，表现形态是自洽的：混格式采集与官方指纹不符、而单格式采集相符，
使用者由此自然把问题定位到「格式相关」的行为上。比较阶段会对两侧 `sampling`
的差异输出提示性 NOTE（不是拒绝），帮助使用者做这个归因。

分配表仍然逐字记录在 manifest 里，因为它是复现与归因所必需的信息：报告要能
回答「这 20 个样本里有几个是流式采的」。

### 5.4 洗牌

分配完成后，把该 cell 的所有请求用 `runtime.seed` 洗牌，决定发出顺序。
`seed` **不进 digest** —— 它只影响请求的时间顺序，不影响每个档位采到多少样本。
两侧用不同 seed 采集的数据仍然可比。

## 6. probes

```yaml
probes:
  onetoken:
    enabled: true
    repeats: 20
    temperature: 1.0
    max_tokens: 16
    context_buckets: [0]
    thinking_effort: null
    min_n: 10                 # 可选；缺省 10

  tokenizer:
    enabled: true
    repeats: 1
    temperature: 0.0
    max_tokens: 16
    context_buckets: [0]
    thinking_effort: null
    min_n: 1                  # 可选；缺省 1

  needle:
    enabled: true
    repeats: 5
    temperature: 0.0
    max_tokens: 64
    context_buckets: [8000, 32000, 128000]
    thinking_effort: null

  think-effort:
    enabled: true
    repeats: 10
    temperature: 1.0
    max_tokens: 2048
    context_buckets: [0]
    # 强度级别写在题目上，不在这里
    # cell 按协议分层（观测单位随协议：openai 系 = tokens，anthropic 系 = chars），
    # 每协议份额 = repeats × weight，须 ≥ min_n 才参与检验 ——
    # weight 小的协议要配更大的 repeats（见 PROBES.md）

  toolcall:
    enabled: true
    repeats: 10
    temperature: 1.0
    max_tokens: 512
    context_buckets: [0]
    thinking_effort: null
```

| 字段 | 说明 |
| :--- | :--- |
| `enabled` | 是否启用。禁用的探针不出现在 CollectionPlan 中 |
| `repeats` | **每 cell** 的总请求数，按比例切分给各 (protocol, transport) |
| `temperature` / `top_p` / `max_tokens` | 采样参数，进 digest |
| `context_buckets` | 该探针要跑的上下文长度档位，`0` 表示不加填充 |
| `thinking_effort` | 探针级默认强度；题目上的同名字段优先 |
| `min_n` | 单侧有效样本下限（比较侧判 insufficient 的门槛），进 digest。缺省：onetoken 10、tokenizer 1、needle 1、think-effort 5、toolcall 5。显式给出时必须 ≥ 1。降低它可以让小样本参与统计，但分布估计会更粗糙；tokenizer 的相等性判定不消费该值（单样本即可判），仅随计划记录。判据参数（与 thresholds 同类，严格与子集模式一致）：比较时按 `本地 config > 计划烘焙值 > 缺省` 解析 —— 采集后改本地 config 里的 `min_n` 会影响同 digest 数据的判定；`--allow-subset` 下闸门忽略两侧差异 |

`onetoken` 的温度固定语义为「要采样分布形状」，配置成 0 会让分布退化成单点、
探针失去意义。校验阶段对 `onetoken.temperature < 0.5` 发出警告。

## 7. thresholds

```yaml
thresholds:
  onetoken: 0.15        # 分布距离上限，越大越宽松
  onetoken.0: 0.10      # 可选：档位级覆盖，只对该 context_bucket 生效
  onetoken.8000: 0.20   # 可选：长上下文噪声大，可放宽
  tokenizer: 0.01       # 卡方检验显著性水平 alpha
  needle: 0.01          # 召回率差异 2×2 卡方的 alpha
  think-effort: 0.01    # Mann-Whitney U + Fisher 合并 p 值的 alpha
  toolcall: 0.05        # 双分量共用：JSD 均值上限 + 合法率卡方 alpha
```

键有两种形式：`<probe>` 是探针级兜底；`<probe>.<bucket>` 只对某个
`context_bucket` 档位生效（探针 ID 不含 `.`，因此 `.` 是无歧义分隔符）。
档位键的档位必须出现在该探针的 `context_buckets` 里，否则报错 ——
`onetoken.8001` 这类笔误静默不生效是最坑的失败模式，必须在入口拒绝。

**比较按档位分区**：每个 `context_bucket` 用各自生效的阈值独立判定，
探针级结论取最差档位（rollup 的 `statistic` / `threshold` / `ratio`
即最差档位的值，`ratio > 1 ⟺ fail` 的不变式保持成立）。报告里
多档位探针逐档位展开子判定（stdout 子行、JSON `buckets` 数组、
JUnit 每档位一个 testcase）。某档位样本不足判 inconclusive 时排除该档位、
不污染整体，并在 warning 里点名；全部档位 inconclusive 才整体 inconclusive。

档位键允许只写满所有档位、不写探针级兜底；此时任缺一个档位即报缺阈值。

**工具不提供任何默认阈值。** `compare` / `run` 时，任一被启用探针
（的任一档位）缺阈值即拒绝执行并退出（`exit 2`），指名缺哪个：

```text
ERROR  no threshold for probe onetoken (context_bucket=8000)
  用 --threshold onetoken.8000=<value> 或 --threshold onetoken=<value>（兜底）提供，
  或在配置文件的 thresholds 段中设置。本工具不提供默认阈值：
  合适的阈值取决于模型自身的随机性、部署环境与你的容忍度，只能由使用者标定。
exit 2
```

理由：一个阈值意味着「多大的差异算异常」。给一个看起来合理的默认值，等于替
使用者做了一个他不知道自己在做的判断 —— 一个天生分布很散的模型用 0.15 会误报，
一个非常稳定的模型用 0.15 会漏报，而使用者看到 `pass` 不会想到去质疑默认值。

同一档位内命令行覆盖配置文件，档位级覆盖探针级，四级优先级：

```
--threshold <probe>.<bucket>=<v>  >  thresholds.<probe>.<bucket>
  >  --threshold <probe>=<v>      >  thresholds.<probe>
```

```bash
tv compare a.rawdata.jsonl.gz b.rawdata.jsonl.gz \
  --threshold onetoken=0.12 --threshold onetoken.8000=0.2 --threshold tokenizer=0.01
```

报告的每个探针行（及档位子行）都会印出实际取值与来源（`config` / `flag`），
所以永远不存在「不知道这个判定是按什么标准做的」。

### 标定起点

下表是从参考实现的生产使用中观察到的经验值，**不是默认值**，仅作首次标定的起点。
合适的阈值需要用同一模型的多次采集数据自行确定：对同一个已知可信的端点采集两次，
用得到的统计量作为「正常波动」的量级，再在它之上留出余量。

| 探针 | 统计量 | 经验起点 | 说明 |
| :--- | :--- | :--- | :--- |
| `onetoken` | 分布距离 | 0.10 – 0.15 | 用于告警时取更严的一端 |
| `tokenizer` | alpha | 0.01 | 整数比较，噪声极低 |
| `needle` | alpha | 0.01 | 埋点二值，噪声偏大；埋点单元越多功效越高 |
| `think-effort` | alpha | 0.01 – 0.05 | Mann-Whitney U 逐 cell 检验后 Fisher 合并 |
| `toolcall` | 距离 + alpha | 0.05 – 0.15 | 双分量共用一个阈值，见下 |

`toolcall` 的阈值一值两用：工具选择分布的 JSD 均值超过它判超（距离语义），
参数合法率的 2×2 卡方 p 值低于它判超（alpha 语义），任一分量超阈即 fail。
取值时先想清楚哪个语义是你要卡的主线：**以合法率为主线时应取 0.01 – 0.05**
—— 配到区间上沿 0.15 时，alpha 分量实际是相当宽松的显著性水平，
合法率崩塌可能漏报；以 JSD 为主线时才适合 0.05 – 0.15 的宽松端。

**阈值不参与可比性判定。** 它不写进采集计划、不参与 digest，所以改阈值不会让
你手里的 rawData 与官方基线变得不可比。可比性只由采集参数决定，判据是独立的一层。

**按档位分区的统计代价。** p 值类探针（`tokenizer` / `needle` / `think-effort`）
此前把各档位的样本池化成一次检验；分区后每个档位独立检验，单次样本量下降、
功效降低（`lowPowerCells` 警告更易触发），且 B 个档位各按 α 判定时探针级
族错误率约为 `1-(1-α)^B`（α=0.01、2 档 ≈ 2%）。这是有意的取舍：池化会把
「短上下文正常、长上下文回归」稀释掉；需要补偿时给长上下文档位单独设更小的 α。
距离类探针（`onetoken` / `toolcall`）分区只是去掉跨档位求均值的稀释，无功效问题。

## 8. runtime

```yaml
runtime:
  max_concurrency: 4        # 并发上限
  timeout_sec: 120          # 单请求超时
  min_interval_ms: 0        # 请求最小间隔，用于遵守端点速率限制
  max_attempts: 3           # 单题上限，含首次，即最多重试 2 次
  seed: 20260904            # 洗牌种子
  raw_level: digest         # digest | full
  backoff_base_ms: 500      # 重试退避基准：第 n 次重试前等 base << n 毫秒
  backoff_max_ms: 30000     # 退避封顶
```

全部**不进 digest**：它们影响采集耗时与失败率，不影响采到什么样的样本。

**没有全局请求数 / token 数 / 时长预算闸门。** 总请求量在采集前完全由配置决定
（题数 × 档位数 × 协议数 × 传输数 × repeats），`--dry-run` 先把这个数与 token
估算印出来，由使用者决定是否执行。预算是配置阶段的责任，不是运行时的隐式截断 ——
运行时截断会产生一份「不完整但看起来完整」的数据，比超支更糟。

`raw_level`：

| 值 | 保留内容 | 用途 |
| :--- | :--- | :--- |
| `digest` | 观测值 + 原文哈希 | 官方发布默认。可安全公开 |
| `full` | 追加原始响应文本与 SSE chunk | 本地审计与排查 |

**硬约束：所有比较统计必须仅凭 `digest` 档可算。** 这是 `Observe` 前移到采集
阶段的直接原因。

## 9. padding

```yaml
padding:
  seed: 12345               # 可选；缺省 12345
```

`context_buckets` 中大于 0 的档位会在 prompt 前拼接确定性填充文本，
文本由 `(bucket, seed)` 唯一决定（算法见 `padding_algo_version`，当前 `padding/v2`）。

- **seed 进 digest**（记录在 collection_plan 的 `padding_seed` 字段）：
  同一 bucket 在两次采集中必须产出逐字节相同的填充文本，否则送入端点的
  内容不同，样本不可比。跨环境一致性由「配置相同 → digest 相同 → 闸门保证」
  承接，两侧 seed 不同时比较会被 digest 闸门拒绝。
- 缺省值 12345 是 `padding/v1` 时代的固定种子，不写该段即可复现旧行为。
- 填充词表（47 个中性英文词）仍在代码中，不开放配置：它属于填充算法的
  一部分，改动会递增 `padding_algo_version`。

## 10. 校验规则

配置加载后按序校验，任一失败即 `exit 2` 并指明字段路径。

**结构性**

1. `version` 必须为已知值
2. `base_url` 非空且为合法 URL
3. `api_key_env` 非空，且该环境变量已设置（`collect` / `run` 时）
4. `model` 非空
5. `protocols` 至少一项，`id` 属于已知集合且不重复
6. 至少一个协议 `weight > 0`
7. 每个协议的 `transport` 至少一项 > 0
8. `suite.path` 必填且文件存在；给了 `sha256` 则必须校验通过

**探针与题库**

9. 至少一个探针 `enabled: true`
10. 每个启用探针的 `repeats ≥ 1`、`context_buckets` 非空
11. `min_n` 显式给出时必须 ≥ 1
12. 题库中存在该探针的题目
13. 题目用到的每个 `thinking_effort` 级别，在**所有** `weight > 0` 的协议的
    `enabled.effort_map` 中都有映射
14. `context_buckets` 严格递增且首项为 0 或正整数

**注入与冲突**

15. `body_overrides` 不含保留字段（`model`、各协议消息体字段、输出契约的 `system`、toolcall 的 `tools`）
16. 协议的 `enabled.body_overrides` 与 `request.temperature_field` 不指向
    同一路径
17. `${thinking_effort}` 只出现在存在 `effort_map` 的 `enabled` 块中

**判据**（仅 `compare` / `run`）

18. 每个启用探针的每个档位都有阈值（档位键或探针级兜底，配置或命令行）；
    档位键 `<probe>.<bucket>` 的档位须在该探针的 `context_buckets` 内，
    后缀须为整数（未知/未启用探针的键宽容忽略）
19. 阈值为正数；alpha 类阈值在 `(0, 1)` 内（档位键按探针归类）

**警告（不阻断）**

- `onetoken.temperature < 0.5`：分布可能退化，探针有效性下降
- 某探针的 `repeats` 低于其生效 `min_n`（配置值或缺省值）：可能大量 cell 被判 insufficient
- `think-effort` 的某协议期望样本（`repeats` × 权重份额）低于 `min_n`：
  cell 按协议分层，份额不足的协议其 cell 全部判 insufficient
- 声明 `capabilities` 与所用探针明显不匹配（如启用 `toolcall` 但声明
  `tools: false`）：该探针将无任何请求，比较时表现为缺失

## 11. 完整示例

```yaml
version: 1

target:
  base_url: https://api.example.com
  api_key_env: TARGET_API_KEY
  model: example-model
  protocols:
    - id: openai-chat
      weight: 0.7
      capabilities: { stream: true, tools: true, thinking: true, context_window: 131072 }
      transport: { stream: 0.5, non_stream: 0.5 }
      thinking:
        disabled:
          body_overrides: {}
        enabled:
          effort_map: { low: low, medium: medium, high: high }
          body_overrides:
            reasoning_effort: "${thinking_effort}"
    - id: anthropic-messages
      weight: 0.3
      capabilities: { stream: true, tools: true, thinking: true, context_window: 200000 }
      transport: { stream: 1.0, non_stream: 0.0 }
      thinking:
        disabled:
          body_overrides: {}
        enabled:
          effort_map: { low: 1024, medium: 4096, high: 16384 }
          body_overrides:
            thinking:
              type: enabled
              budget_tokens: "${thinking_effort}"
  request:
    temperature_field: temperature
    body_overrides: {}

suite:
  path: ./suite.yaml
  sha256: ...

probes:
  onetoken:
    enabled: true
    repeats: 20
    temperature: 1.0
    max_tokens: 16
    context_buckets: [0, 32000]
  tokenizer:
    enabled: true
    repeats: 1
    temperature: 0.0
    max_tokens: 16
    context_buckets: [0]
  needle:
    enabled: true
    repeats: 5
    temperature: 0.0
    max_tokens: 256             # 多埋点题逐行输出全部标记，留出余量
    context_buckets: [32000, 128000]
  think-effort:
    enabled: true
    repeats: 10
    temperature: 1.0
    max_tokens: 2048
    context_buckets: [0]
  toolcall:
    enabled: false

thresholds:
  onetoken: 0.15        # 分布距离上限（探针级兜底）
  onetoken.32000: 0.2   # 档位级覆盖：长上下文放宽（须在 context_buckets 内）
  tokenizer: 0.01       # alpha
  needle: 0.01          # alpha
  think-effort: 0.01    # alpha（Mann-Whitney U + Fisher 合并）
  toolcall: 0.05        # 双分量共用（JSD 距离上限 + 合法率 alpha）

runtime:
  max_concurrency: 4
  timeout_sec: 120
  max_attempts: 3
  seed: 20260904
  raw_level: digest
  backoff_base_ms: 500
  backoff_max_ms: 30000

padding:
  seed: 12345
```

仓库根目录的 [`config.example.yaml`](../config.example.yaml) 是一份带注释的
可直接运行示例（配 `suites/v1.example.yaml`）。
