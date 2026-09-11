# 题库（suite）规范

> 题库定义「测什么题」。采集参数（重复次数、温度、档位）在配置里，
> 判据（阈值）在比较时，三者互不混淆。
>
> 配套阅读：[PROBES.md](./PROBES.md)（各探针的机理与题目设计动机）、
> [SPEC-CONFIG.md](./SPEC-CONFIG.md)（`suite` 配置段与校验规则）。

## 1. 定位

一份 suite 是一个 YAML 文件，包含：

- 版本标识 `suite_version`
- 每个探针的题目集合：题目文本、标识、归一化规则、思考强度级别、
  以及探针特有的附加字段（期望答案、工具定义等）

**suite 一律由外部文件提供，工具不内置题库。** 配置里 `suite.path` 指向该文件，
可用 `sha256` 校验完整性。

这个决定是刻意的：题库是检测内容本身，把它编进二进制会让「改题目」变成「发版本」，
也会让不同版本工具产出的 rawData 之间多一层隐式差异。外部文件让题库可审查、
可 diff、可随官方基线一起分发。

**官方发布的参考 rawData 配套发布其 suite 文件**（或声明所用 suite 的版本与
digest）。这让所有官方基线之间可比，也让任何使用者都能用同一份题库去对标。
用自定义 suite 采集的数据，只能与同样使用该 suite 采集的数据比较 ——
`suite_version` 与 `question_set_digest` 都参与采集计划的 digest，两侧不一致
会被直接拒绝并指出冲突。

## 2. 文件格式

```yaml
suite_version: v2

output_contract:                 # 输出契约（system prompt 强约束），见 §4.1
  field: answer
  system_prompt: '请只回答一个合法的 JSON 对象，格式为 {"answer": "<your answer>"}，不要输出其它任何内容。其中值必须是简短的单一答案（一个词或一个数字），不要解释。'

normalize:                       # 归一化空间：词表由题库提供，见 §4
  maps:
    color:
      red:    [red, crimson, 红, 红色, rouge, rot, rojo]
      blue:   [blue, navy, 蓝, 蓝色, bleu, blau, azul]
      green:  [green, 绿, 绿色, vert, grün, verde]
      yellow: [yellow, gold, 黄, 黄色, jaune, gelb, amarillo]
      black:  [black, 黑, 黑色, noir, schwarz, negro]
      white:  [white, 白, 白色, blanc, weiß, blanco]
      purple: [purple, violet, 紫, 紫色, lila, morado]
      orange: [orange, 橙, 橙色, naranja, arancione]
      pink:   [pink, 粉, 粉色, 粉红, rose, rosa]
      gray:   [gray, grey, 灰, 灰色, gris, grau]
      brown:  [brown, 棕, 棕色, 褐, marron, braun]
  # digits:  可在此扩展 number 规则的字符表（如带圈数字 ⑦: 7）
  # letters: 可在此扩展 letter 规则的字符表（如变音字母 é: e）

probes:
  onetoken:
    normalize: number            # 该探针的答案归一化规则，见 §4
    thinking_effort: null        # 探针级默认；题目上的同名字段优先
    items:
      - id: ot.v1.001
        prompt: "说出一个 1 到 100 之间的随机整数，只输出这个数字。"
      - id: ot.v1.002
        prompt: "随机说一个英文字母，只输出这个字母。"
      - id: ot.v1.003
        prompt: "随机说一种颜色，只输出颜色名。"
        normalize: color         # 题目级覆盖

  tokenizer:
    normalize: none              # 该探针不看答案文本，只看 usage
    items:
      - id: tk.v1.001
        prompt: "重复以下内容一次：こんにちは、🎉、ｕｌｌｗｉｄｔｈ。"
      - id: tk.v1.002
        prompt: |
          {"a": [1, 2, 3], "b": {"c": "emoji 🚀🚀🚀"}}
          请原样输出这段 JSON。

  needle:
    normalize: none
    items:
      - id: nd.v1.001
        prompt: "读完上文后，逐行输出你找到的全部标记。"
        needle_positions: [0.25, 0.5, 0.75]   # 三个埋点；标记派生见 §5
      - id: nd.v1.002
        prompt: "读完上文后，输出你找到的标记。"
        needle_positions: [0.5]               # 缺省即 [0.5]，可省略

  think-effort:
    normalize: none              # 观测思考 token 数，不看答案文本
    items:
      - id: te.v1.001
        prompt: "估算 17 乘 23，先思考再给出结果。"
        thinking_effort: low
      - id: te.v1.002
        prompt: "比较两种排序算法在百万级数据下的表现，先思考再回答。"
        thinking_effort: high

  toolcall:
    normalize: none
    tools:                       # 探针级工具集；题目可覆盖
      - name: get_weather
        description: 获取某城市当前天气
        parameters:
          type: object
          properties:
            city: { type: string }
          required: [city]
    items:
      - id: tc.v1.001
        prompt: "北京现在多少度？"
      - id: tc.v1.002
        prompt: "上海和广州哪个更热？分别查一下。"   # 期望并行调用
        expect_parallel: true
```

### 字段说明

| 字段 | 层级 | 说明 |
| :--- | :--- | :--- |
| `suite_version` | 顶层 | 语义版本。题目集合或语义变化必须递增 |
| `output_contract` | 顶层 | 输出契约（system prompt 强约束），见 §4.1。存在 normalize ≠ none 的题目时必填 |
| `probes.<id>` | 探针 | 未列出的探针视为该 suite 不提供题目 |
| `normalize` | 探针 / 题目 | 答案归一化规则，题目级覆盖探针级 |
| `thinking_effort` | 探针 / 题目 | 符号级别 `low`/`medium`/`high`/`null` |
| `items[].id` | 题目 | suite 内唯一。**跨 suite 也必须唯一**，见 §3 |
| `items[].prompt` | 题目 | 完整用户消息文本 |
| `items[].needle_positions` | 题目 | 仅 `needle`：埋点相对位置列表，取值 (0,1) 开区间，缺省 `[0.5]`。一个位置 = 一个埋点 = 比较侧一个统计单元 |
| `items[].tools` | 题目 | 覆盖探针级工具集（仅 `toolcall`） |
| `items[].expect_parallel` | 题目 | 仅 `toolcall`，标注期望并行调用（进报告，不进判定） |

`needle` 的题目**不含标记文本**。标记由 (`id`, 位置序号) 经哈希确定性派生，
在采集时埋入语料，见 §5。

## 3. 题目标识与 digest

`question_set_digest` 由所有启用探针的题目 `id` 集合（排序后）计算，与
`suite_version` 一起进入采集计划的 digest。

因此：

- **改题目文本**必须递增 `suite_version`（否则同一 id 对应不同文本，digest 不变，
  旧数据被静默错配）。
- **增删题目**会改变 id 集合，digest 自动变化，无需人工干预。
- **自定义 suite 建议使用自己的 id 前缀**（如 `mycorp.ot.001`），避免与官方
  发布的 suite 的 id 碰撞后误以为可比。

## 4. 归一化规则

`normalize` 决定原始响应文本如何压成可统计的答案值。规则在采集期的 `Observe`
步骤执行。

规则分两类，归属不同：

- **机理规则**（`number` / `letter` / `word` / `none`）：提取算法在代码里，
  因为算法不是题目内容；
- **词表规则**：词表由题库顶层 `normalize.maps` 段提供，**程序内不写死任何词表**。
  词表属于题目内容（哪些同义词算同一个答案，本身就是题库作者的判断），
  随题库外置、可审查、可自定义；规则名任意（`color` 只是惯例），
  题目/探针的 `normalize` 引用了 `maps` 里定义过的名字即生效。

| 规则 | 行为 |
| :--- | :--- |
| `number` | 提取第一个数字（阿拉伯/全角/阿拉伯-印度数字与中文复合数词统一，
  「四十二」→ `42`）；可用 `normalize.digits` 扩展字符表 |
| `letter` | 提取第一个字母，转小写（全角折叠）；可用 `normalize.letters` 扩展 |
| `word` | 取第一个词，转小写 |
| `none` | 不归一化；探针自行从响应中提取观测量（token 数、思考 token 数、
  工具调用、标记是否出现） |
| 词表规则 | 大小写不敏感子串匹配 → 规范值；多个命中时**最长同义词优先**、
  同长按规范名字典序，结果确定（复合词如 "red-orange" 恒归到 orange）；
  未命中保留原文 |

整个 `normalize` 段经采集计划进入 `plan_digest`：**改词表 = 改观测语义**，
两侧词表不同会在 digest 闸门被拒绝 —— 不需要人工记得递增版本号。
机理规则的语义变更仍须递增对应探针的 `probe_version`（它是代码行为，
不体现在题库文件里）。

> 历史教训：归一化会小写化，而长上下文标记匹配曾是大小写敏感的，导致命中率恒为 0、
> 判定恒为 pass。匹配逻辑放在 `Observe` 内并产出 `matched` 布尔值后，这类错误
> 在第一次采集就会显形。

### 4.1 输出契约（system prompt 强约束）

归一化是「尽力从自由文本里抠出答案」，模型多说一句话就可能污染提取结果
（比如「我猜是 55，但也可能是 6」会提取到 55）。输出契约把约束前移到请求侧：

- **应用规则是机械的，写在代码里，不由题目逐条声明**：凡生效 `normalize`
  规则 ≠ `none`/空的探针（即需要从回复文本提取答案的），采集时自动注入
  `output_contract.system_prompt` 作为 system 消息（anthropic-messages 协议
  为顶层 `system` 字段），要求模型只回答 `{"<field>": "<your answer>"}`。
  当前命中的是 onetoken；未来新探针只要声明了 normalize 就自动获得约束，
  零适配成本。
- `normalize = none` 的探针（如 tokenizer，观测服务端上报的
  `usage.prompt_tokens`）**不发送 system prompt** —— 多一条 system 消息会
  改变输入 token 计数，恰好污染它的观测量。
- **解析端三级容错**（`internal/collect/jsonanswer.go`）：剥 markdown 围栏 →
  整段 JSON 解析 → 截取首 `{` 到末 `}` 片段；值兼容字符串与数字。全部失败
  回退原文，走既有归一化兜底 —— 契约失效不判错，只是退化为无约束时的行为。
- 契约文本由题库提供（与词表同一哲学：属于检测内容，外置、可审查、可自定义），
  **整份题库只有一份契约文本，不做多语言变体**；中英题面共用。题面里的
  「只输出这个数字」类指令保留，与 system prompt 双保险。
- 契约（`field` + `system_prompt` 生效值）经采集计划进入 `plan_digest`：
  两侧契约不同 = 对模型的格式约束不同 = 答案分布不可比，闸门直接拒绝。
- `system` 是请求体保留字段（与 `model`/`messages`/`input` 同级），
  `body_overrides` 不许覆盖，配置校验与 adapter 双保险。

## 5. needle 的标记与语料

标记由 (题目 `id`, 埋点位置序号) 哈希派生，**不以明文出现在 suite 文件里**；
埋入位置由题目显式声明，支持一题多埋点：

- 标记 = (`id` + 位置序号) 经 FNV-1a 哈希后从一组不易混淆的字符
  （`ABCDEFGHJKLMNPQRSTUVWXYZ23456789`，去掉 I/O/0/1）取 8 位，加固定前缀
  `NEEDLE-`
- 埋入位置 = 题目 `needle_positions` 声明的比例值列表（(0,1) 开区间），
  如 `[0.25, 0.5, 0.75]`；一个位置埋一个独立标记，加载时升序钳制
- 埋点包装成「读到该标记请原样输出：NEEDLE-XXXX」指令插入语料，
  避免模型把裸标记当语料噪声忽略

语料（长上下文填充文本）在采集时按 `context_bucket` 生成并缓存，suite 只负责
题目、位置与 prompt 模板。语料生成算法的版本号进入采集计划
（`padding_algo_version`），因为它影响实际送进模型的文本；位置声明随题目
进入 `question_set_digest` 所辖的题库版本。

这样公开 suite 或公开 rawData 都不会泄露标记内容，被测端点无法预知。

## 6. 编写与使用 suite 的流程

1. 从零编写，或从官方发布的 suite 文件复制作为起点
2. 给自己的题目使用独立 id 前缀（如 `mycorp.ot.001`）
3. 递增 `suite_version`
4. 在配置中填 `suite.path`，建议同时填 `sha256`
5. 先 `--dry-run` 看请求数与 token 估算，确认预算
6. 采集一份自己的基线，用于后续对标与阈值标定

**自定义 suite 的结论只在与使用同一 suite 的数据比较时成立。** 报告与 rawData
都会带上 `suite_version` 与 `question_set_digest`，混用会在 digest 闸门被拒绝。

## 7. 校验

配置加载时对 suite 执行的检查（见 SPEC-CONFIG.md §10）：

- `suite.path` 必填且文件存在；给了 `sha256` 必须校验通过
- 每个启用探针在 suite 中有题目
- 题目用到的每个 `thinking_effort` 级别，在所有 `weight > 0` 协议的
  `effort_map` 中都有映射
- `id` 在 suite 内唯一
- `normalize` 取值属于机理规则集合（number/letter/word/none）或题库
  `normalize.maps` 已定义的词表规则；`normalize` 段自身的键值合法性
  （单字符键、非空词表）也在校验范围
- `needle_positions` 的每个值在 (0,1) 开区间内（埋点是占语料长度的比例，
  首尾 0/1 会让标记贴在消息边界，语义上不是「埋在语料中」）
- 工具集（探针级与题目级）的 `name` 非空且集合内唯一；`parameters` 非空
  且声明为 object 形态的 JSON schema
- 存在生效 `normalize` ≠ `none` 的题目时，`output_contract` 段必须存在且
  `system_prompt` 非空（见 §4.1）—— 强约束与归一化提取是一对配套语义
