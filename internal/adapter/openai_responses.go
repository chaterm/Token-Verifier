package adapter

// openaiResponses 适配器：Responses 风格（openai-responses）。
// 端点形态近似 POST {base}/responses，Bearer 头；请求体用 input 数组而非
// messages，usage 字段为 input_tokens/output_tokens，思考 token 在
// output_tokens_details.reasoning_tokens。
// reasoning 默认开启（effort:high + summary:auto）：think-effort 探针需要
// 服务端上报思考 token 数，默认不开会让该协议在这类端点上永远采不到观测；
// 具体档位仍由 thinking 块的 ${thinking_effort} 注入覆盖。

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// openaiResponsesAdapter 对应协议 id "openai-responses"。
type openaiResponsesAdapter struct{}

const responsesDefaultPath = "/responses"

func (openaiResponsesAdapter) ID() string { return "openai-responses" }

// Render 构造 Responses 请求。input 是保留字段，body_overrides 不能覆盖。
func (a openaiResponsesAdapter) Render(baseURL, apiKey string, lr LogicalRequest, tb ThinkingBlock) (*http.Request, error) {
	input := []map[string]any{}
	if lr.System != "" {
		input = append(input, map[string]any{
			"role":    "system",
			"content": []map[string]any{{"type": "input_text", "text": lr.System}},
		})
	}
	input = append(input, map[string]any{
		"role":    "user",
		"content": []map[string]any{{"type": "input_text", "text": lr.Padding + lr.Prompt}},
	})
	skeleton := map[string]any{
		"model":  lr.Model,
		"input":  input,
		"stream": lr.Stream,
		// 默认开启思考并要求上报摘要（think-effort 探针依赖 reasoning_tokens
		// 上报）。renderBody 的 overrides 是整键替换：覆盖 reasoning 时须
		// 给出完整块（config.example 的 thinking 注入示例即如此）。
		"reasoning": map[string]any{"effort": "high", "summary": "auto"},
	}
	if lr.MaxTokens != nil {
		skeleton["max_output_tokens"] = *lr.MaxTokens
	}
	if lr.TopP != nil {
		skeleton["top_p"] = *lr.TopP
	}
	if len(lr.Tools) > 0 {
		skeleton["tools"] = responsesToolsPayload(lr.Tools)
	}
	payload, err := renderBody(skeleton, tb.BodyOverrides, lr.TemperatureField, lr.Temperature)
	if err != nil {
		return nil, fmt.Errorf("序列化请求体失败: %w", err)
	}
	path := lr.Path
	if path == "" {
		path = responsesDefaultPath
	}
	return postJSON(joinURL(baseURL, path), apiKey, "application/json", payload)
}

// responsesToolsPayload 工具集的 Responses 形态：扁平 function 对象
// （name/description/parameters 直接在顶层，无 function 包装）。
func responsesToolsPayload(tools []ToolDef) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		out = append(out, map[string]any{
			"type":        "function",
			"name":        t.Name,
			"description": t.Description,
			"parameters":  t.Parameters,
		})
	}
	return out
}

// responsesOutputItem 响应 output 数组的元素（message / reasoning / function_call）。
type responsesOutputItem struct {
	Type string `json:"type"`
	// message
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	// reasoning
	Summary []struct {
		Text string `json:"text"`
	} `json:"summary"`
	// function_call
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// responsesUsage Responses 的 usage 形态。
type responsesUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	InputTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

// normalize 归一为统一 Usage。
func (u *responsesUsage) normalize() Usage {
	out := Usage{Prompt: u.InputTokens, Completion: u.OutputTokens}
	if u.InputTokensDetails != nil {
		out.Cached = u.InputTokensDetails.CachedTokens
	}
	if u.OutputTokensDetails != nil {
		out.Reasoning = u.OutputTokensDetails.ReasoningTokens
	}
	return out
}

// responsesResponse 非流式响应结构。
type responsesResponse struct {
	Status string                `json:"status"`
	Output []responsesOutputItem `json:"output"`
	Usage  *responsesUsage       `json:"usage"`
	Error  errorField            `json:"error"`
}

// ParseOnce 解析非流式响应：output 数组按类型归并。
func (openaiResponsesAdapter) ParseOnce(resp *http.Response) (Response, error) {
	body, err := readAll(resp)
	if err != nil {
		return Response{}, fmt.Errorf("读取响应体失败: %w", err)
	}
	var out responsesResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return Response{}, &ProtocolError{Kind: "parse", Detail: fmt.Sprintf("响应不是合法 JSON: %v", err)}
	}
	if out.Error.IsError() {
		return Response{}, &ProtocolError{Kind: "protocol", Detail: out.Error.Message}
	}
	var r Response
	r.FinishReason = out.Status
	for _, item := range out.Output {
		switch item.Type {
		case "message":
			for _, c := range item.Content {
				if c.Type == "output_text" {
					r.Content += c.Text
				}
			}
		case "reasoning":
			for _, s := range item.Summary {
				r.ReasoningContent += s.Text
			}
		case "function_call":
			r.ToolCalls = append(r.ToolCalls, ToolCall{
				Name: item.Name,
				Args: json.RawMessage(item.Arguments),
			})
		}
	}
	if out.Usage != nil {
		r.Usage = out.Usage.normalize()
	}
	return r, nil
}

// responsesSSEEvent 流式事件信封（Responses 用带类型的 JSON 事件，非 [DONE] 结尾）。
type responsesSSEEvent struct {
	Type string `json:"type"`
	// response.output_text.delta / response.reasoning_summary_text.delta
	Delta string `json:"delta"`
	// response.output_item.done
	Item *responsesOutputItem `json:"item"`
	// response.completed / response.failed / response.incomplete
	Response *responsesResponse `json:"response"`
	Error    errorField         `json:"error"`
}

// ParseStream 解析 Responses 的 SSE 事件流。
func (openaiResponsesAdapter) ParseStream(resp *http.Response, onChunk func(Chunk)) (Response, error) {
	defer func() { _ = resp.Body.Close() }()
	var r Response
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			onChunk(Chunk{Done: true})
			break
		}
		var ev responsesSSEEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			return r, &ProtocolError{Kind: "parse", Detail: fmt.Sprintf("SSE 事件不是合法 JSON: %v", err)}
		}
		if ev.Error.IsError() {
			return r, &ProtocolError{Kind: "protocol", Detail: ev.Error.Message}
		}
		switch ev.Type {
		case "response.output_text.delta":
			r.Content += ev.Delta
			onChunk(Chunk{Delta: ev.Delta})
		case "response.reasoning_summary_text.delta":
			r.ReasoningContent += ev.Delta
			onChunk(Chunk{Reasoning: ev.Delta})
		case "response.output_item.done":
			if ev.Item != nil && ev.Item.Type == "function_call" {
				r.ToolCalls = append(r.ToolCalls, ToolCall{
					Name: ev.Item.Name,
					Args: json.RawMessage(ev.Item.Arguments),
				})
			}
		case "response.completed", "response.incomplete":
			if ev.Response != nil {
				if ev.Response.Status != "" {
					r.FinishReason = ev.Response.Status
				}
				if ev.Response.Usage != nil {
					u := ev.Response.Usage.normalize()
					r.Usage = u
					onChunk(Chunk{Usage: &u})
				}
			}
			onChunk(Chunk{Done: true})
		case "response.failed":
			msg := "响应失败"
			if ev.Response != nil && ev.Response.Error.IsError() {
				msg = ev.Response.Error.Message
			}
			return r, &ProtocolError{Kind: "protocol", Detail: msg}
		}
	}
	if err := sc.Err(); err != nil {
		return r, &ProtocolError{Kind: "parse", Detail: fmt.Sprintf("读取 SSE 流失败: %v", err)}
	}
	return r, nil
}
