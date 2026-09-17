// Package adapter 封装协议差异：请求构造（Render）与响应解析（ParseOnce/ParseStream）。
// Adapter 是唯一知道消息体长什么样、usage 字段叫什么的地方；
// 探针只看到统一的 Response。契约见 docs/ARCHITECTURE.md §3。
package adapter

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/chaterm/token-verifier/internal/config"
)

// LogicalRequest 一次逻辑请求的语义内容，与协议无关。
type LogicalRequest struct {
	Model string
	// System 输出契约的 system prompt（SPEC-SUITE）；空 = 不发送。
	System      string
	Prompt      string // 用户消息文本
	Padding     string // 长上下文填充文本（bucket>0 时非空，前缀在 prompt 前）
	MaxTokens   *int
	Temperature *float64
	TopP        *float64
	Stream      bool
	// Path 覆盖该协议的默认请求路径（config 的 protocols[].path）；空 = 协议默认。
	Path string
	// TemperatureField 温度的点路径（SPEC-CONFIG §4.3）；空 = "temperature"。
	TemperatureField string
	// Tools toolcall 探针的工具集（来自 suite）；nil/空 = 不发送。
	// 各协议的字段名与包装形态由适配器掌控，tools 是请求体保留字段。
	Tools []ToolDef
}

// ToolDef 协议无关的工具定义（suite 侧加载后传入）。
type ToolDef struct {
	Name        string
	Description string
	// Parameters JSON schema（object 形态），原样进请求体。
	Parameters map[string]any
}

// ThinkingBlock 已解析的请求体注入：target.request.body_overrides 与协议
// thinking 块按合并顺序（后者覆盖前者）深合并、${thinking_effort} 已展开的
// 结果（SPEC-CONFIG §4.2）。由 collect 层解析后传入。
type ThinkingBlock struct {
	BodyOverrides map[string]any // nil = 无注入
}

// Response 协议无关的统一响应。
type Response struct {
	Content          string
	ReasoningContent string
	FinishReason     string
	Usage            Usage // Prompt/Completion/Reasoning/Cached 已归一
	ToolCalls        []ToolCall
}

// ToolCall 一次工具调用的归一形态。Args 为参数 JSON 原文
// （流式增量拼接的结果），解析失败时保留原始片段供 args_valid 判定。
type ToolCall struct {
	Name string
	Args json.RawMessage
}

// Usage 归一后的 token 计数。
type Usage struct {
	Prompt     int
	Completion int
	Reasoning  int
	Cached     int
}

// Chunk 流式解析过程中的事件。
type Chunk struct {
	Delta     string // 本轮新增的 content 增量
	Reasoning string // 本轮新增的 reasoning 增量（若有）
	Done      bool   // 流结束
	Usage     *Usage // 若事件携带 usage
}

// Adapter 协议适配器接口。
type Adapter interface {
	ID() string
	// Render 构造 HTTP 请求。baseURL 为配置的端点基址；apiKey 明文只在
	// 本函数内用于请求头，绝不写入任何落盘内容。
	Render(baseURL, apiKey string, lr LogicalRequest, tb ThinkingBlock) (*http.Request, error)
	// ParseOnce 解析非流式响应（HTTP 200）。
	ParseOnce(resp *http.Response) (Response, error)
	// ParseStream 解析流式响应。回调按序收到每个 chunk；返回合并后的响应。
	ParseStream(resp *http.Response, onChunk func(Chunk)) (Response, error)
}

// registry 已实现协议。
var registry = map[string]Adapter{
	"openai-chat":        openaiChatAdapter{},
	"anthropic-messages": anthropicAdapter{},
	"openai-responses":   openaiResponsesAdapter{},
}

// Get 按协议 id 查适配器；未实现的返回 false。
func Get(id string) (Adapter, bool) {
	a, ok := registry[id]
	return a, ok
}

// 思考量观测单位（think-effort 探针）。单位由协议**静态**决定，不做运行时
// 回退 —— 同一 cell 内两侧单位必须一致，否则秩检验失效：
//   - "tokens"：usage 上报独立思考 token 数（openai-chat 的
//     completion_tokens_details.reasoning_tokens、openai-responses 的
//     output_tokens_details.reasoning_tokens）。注意 openai-responses 的
//     ReasoningContent 是摘要不是原始思考，字数在该协议下不代表思考量。
//   - "chars"：usage 无独立思考字段，改用交付的思考文本 rune 数
//     （anthropic-messages：思考 token 折在 output_tokens 里，但 thinking
//     块/思考流完整交付）。
//
// cell_key 含 protocol（与 tokenizer 同构），单位一致性由「协议→单位静态
// 映射 + 按协议分层」保证。未知协议按 tokens 处理（多数协议走 openai 风格
// usage），新增适配器时应显式登记。
func ReasoningUnit(protocolID string) string {
	if protocolID == "anthropic-messages" {
		return "chars"
	}
	return "tokens"
}

// ProtocolError 带错误分类的协议错误，transport 据此写 error_kind。
type ProtocolError struct {
	Kind   string // protocol | parse
	Detail string
}

func (e *ProtocolError) Error() string { return e.Detail }

// errorField 响应/SSE 事件里 "error" 字段的容错形态。标准 API 的错误是
// 对象 {"message":...}；AWS Bedrock 网关在成功响应上附加 "error":""
// （空字符串）—— 空串/null 不是错误，非空字符串按错误文案处理。
// 零值 = 无错误，直接做值字段用。
type errorField struct {
	Message string
	present bool // 对象形态或非空字符串 = 上报了错误
}

func (e *errorField) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		return nil
	}
	if s[0] == '"' {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		if v != "" {
			e.Message = v
			e.present = true
		}
		return nil
	}
	var obj struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		return err
	}
	e.Message = obj.Message
	e.present = true
	return nil
}

// IsError 是否上报了错误（对象形态恒为真，字符串形态非空才为真）。
func (e *errorField) IsError() bool { return e.present }

// DeepMerge 合并两个 JSON 对象：b 覆盖 a（递归），返回新对象，不改入参。
// 导出供 collect 层按 SPEC-CONFIG §4.2 的顺序合并全局与 thinking 的 overrides。
func DeepMerge(a, b map[string]any) map[string]any {
	out := make(map[string]any, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		bm, okB := v.(map[string]any)
		am, okA := out[k].(map[string]any)
		if okB && okA {
			out[k] = DeepMerge(am, bm)
		} else {
			out[k] = v
		}
	}
	return out
}

// ExpandPlaceholders 递归替换 ${thinking_effort}：整个值恰为占位符 → 整体替换并保留
// 映射值类型；嵌在更长字符串中 → 文本插值。未提供映射时占位符保留原样（不应发生，
// 校验已保证占位符只在有 effort_map 的 enabled 块中出现）。
func ExpandPlaceholders(v any, effortVal any) any {
	switch t := v.(type) {
	case string:
		if effortVal != nil {
			if t == "${thinking_effort}" {
				return effortVal // 类型保留
			}
			if strings.Contains(t, "${thinking_effort}") {
				return strings.ReplaceAll(t, "${thinking_effort}", fmt.Sprintf("%v", effortVal))
			}
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = ExpandPlaceholders(x, effortVal)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = ExpandPlaceholders(x, effortVal)
		}
		return out
	default:
		return v
	}
}

// setDotPath 在目标对象上按点路径写入值（必要时创建中间对象）。
func setDotPath(root map[string]any, dotPath string, val any) {
	segs := strings.Split(dotPath, ".")
	cur := root
	for i, seg := range segs {
		if i == len(segs)-1 {
			cur[seg] = val
			return
		}
		next, ok := cur[seg].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[seg] = next
		}
		cur = next
	}
}

// renderBody 组装请求体 map 并序列化：
// 骨架（结构体）→ 并入 overrides（保留字段忽略，config.ReservedFields
// 单一来源，校验已拦，双保险）→
// 温度按 temperatureField 点路径写入。temperatureField 为空时用 "temperature"。
func renderBody(skeleton map[string]any, overrides map[string]any, temperatureField string, temp *float64) ([]byte, error) {
	m := make(map[string]any, len(skeleton)+len(overrides))
	for k, v := range skeleton {
		m[k] = v
	}
	for k, v := range overrides {
		if config.ReservedFields[k] {
			continue
		}
		m[k] = v
	}
	if temp != nil {
		if temperatureField == "" {
			temperatureField = "temperature"
		}
		setDotPath(m, temperatureField, *temp)
	}
	return json.Marshal(m)
}

// maxResponseBodyBytes 单个响应的字节上限（非流式响应体总量 / 流式内容
// 累积总量）。探针语义只关心极短回答与 usage 计数，512KiB 已远大于任何
// 合法响应；无上限时恶意端点用 gzip 炸弹或无限 SSE 流可以把采集机内存
// 吃到耗尽（io.ReadAll / 逐 chunk 累积都不设防）。超限按 parse 错误处理。
const maxResponseBodyBytes = 512 * 1024

// streamBudget 流式内容累积字节计数器：content / reasoning / 工具参数分片
// 都计入预算。Scanner 的单行上限拦不住「海量小 chunk」的总量攻击，
// 这里在每次累积前检查总量。
type streamBudget struct{ n int }

// add 计入若干增量并检查总量；超限返回 parse 类 ProtocolError（调用方
// 应立即中止解析并返回，不再累积该增量）。
func (b *streamBudget) add(parts ...string) error {
	for _, p := range parts {
		b.n += len(p)
	}
	if b.n > maxResponseBodyBytes {
		return &ProtocolError{Kind: "parse",
			Detail: fmt.Sprintf("流式响应内容总量超过上限 %d 字节，已中止读取", maxResponseBodyBytes)}
	}
	return nil
}

// readAll 读完整响应体，总量封顶 maxResponseBodyBytes。
// 上限按解压后的字节数计（http.Transport 透明解压 gzip），恰好是要
// 防的内存放大面。超限返回 parse 类 ProtocolError。
func readAll(resp *http.Response) ([]byte, error) {
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxResponseBodyBytes {
		return nil, &ProtocolError{Kind: "parse",
			Detail: fmt.Sprintf("响应体超过上限 %d 字节", maxResponseBodyBytes)}
	}
	return b, nil
}
