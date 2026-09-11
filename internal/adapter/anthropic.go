package adapter

// anthropic 适配器：Anthropic Messages 风格（anthropic-messages）。
// 端点形态近似 POST {base}/messages，x-api-key / anthropic-version 头。
// 工具的硬约束：max_tokens 必填、usage 字段名 input_tokens/output_tokens
// 归一为 Usage.Prompt/Completion。
// Anthropic 的 usage 无独立思考 token 字段（思考计入 output_tokens），
// Usage.Reasoning 恒为 0。但 thinking 块（非流式）/ thinking_delta（流式）
// 完整交付，ReasoningContent 可数 —— think-effort 探针对本协议改用
// 思考文本 rune 数做观测（ReasoningUnit = "chars"，见 adapter.go）。

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// anthropicAdapter 对应协议 id "anthropic-messages"。
type anthropicAdapter struct{}

const anthropicDefaultPath = "/messages"

// anthropicVersion 消息 API 版本头。
const anthropicVersion = "2023-06-01"

func (anthropicAdapter) ID() string { return "anthropic-messages" }

type anthropicMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// anthropicResponse 非流式响应结构。
type anthropicResponse struct {
	Content []struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		Thinking string          `json:"thinking"` // thinking 块
		Name     string          `json:"name"`     // tool_use
		Input    json.RawMessage `json:"input"`    // tool_use
	} `json:"content"`
	Usage *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Render 构造 Messages 请求。
func (a anthropicAdapter) Render(baseURL, apiKey string, lr LogicalRequest, tb ThinkingBlock) (*http.Request, error) {
	skeleton := map[string]any{
		"model":    lr.Model,
		"messages": []anthropicMsg{{Role: "user", Content: lr.Padding + lr.Prompt}},
	}
	if lr.System != "" {
		skeleton["system"] = lr.System
	}
	if lr.Stream {
		skeleton["stream"] = true
	}
	// max_tokens 必填；未配置时给个保守上限避免端点 400
	mt := 64000
	if lr.MaxTokens != nil {
		mt = *lr.MaxTokens
	}
	skeleton["max_tokens"] = mt
	if lr.TopP != nil {
		skeleton["top_p"] = *lr.TopP
	}
	if len(lr.Tools) > 0 {
		skeleton["tools"] = anthropicToolsPayload(lr.Tools)
	}
	// 温度按配置的 temperature_field 点路径写入
	payload, err := renderBody(skeleton, tb.BodyOverrides, lr.TemperatureField, lr.Temperature)
	if err != nil {
		return nil, fmt.Errorf("序列化请求体失败: %w", err)
	}
	path := lr.Path
	if path == "" {
		path = anthropicDefaultPath
	}
	req, err := postJSON(joinURL(baseURL, path), "", "application/json", payload)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("x-api-key", apiKey)
	}
	req.Header.Set("anthropic-version", anthropicVersion)
	return req, nil
}

// anthropicToolsPayload 工具集的 Messages 形态：parameters 进 input_schema。
func anthropicToolsPayload(tools []ToolDef) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		out = append(out, map[string]any{
			"name":         t.Name,
			"description":  t.Description,
			"input_schema": t.Parameters,
		})
	}
	return out
}

// ParseOnce 解析非流式响应。
func (anthropicAdapter) ParseOnce(resp *http.Response) (Response, error) {
	body, err := readAll(resp)
	if err != nil {
		return Response{}, fmt.Errorf("读取响应体失败: %w", err)
	}
	var out anthropicResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return Response{}, &ProtocolError{Kind: "parse", Detail: fmt.Sprintf("响应不是合法 JSON: %v", err)}
	}
	if out.Error != nil {
		return Response{}, &ProtocolError{Kind: "protocol", Detail: out.Error.Message}
	}
	var r Response
	for _, c := range out.Content {
		switch c.Type {
		case "text":
			r.Content += c.Text
		case "thinking":
			r.ReasoningContent += c.Thinking
		case "tool_use":
			r.ToolCalls = append(r.ToolCalls, ToolCall{Name: c.Name, Args: c.Input})
		}
	}
	if out.Usage != nil {
		r.Usage.Prompt = out.Usage.InputTokens
		r.Usage.Completion = out.Usage.OutputTokens
	}
	return r, nil
}

// anthropicSSEEvent 流式事件信封。
type anthropicSSEEvent struct {
	Type string `json:"type"`
	// content_block_start
	Index        *int `json:"index"`
	ContentBlock *struct {
		Type string `json:"type"`
		Name string `json:"name"` // tool_use
	} `json:"content_block"`
	// content_block_delta
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`     // thinking_delta
		PartialJSON string `json:"partial_json"` // input_json_delta
	} `json:"delta"`
	// message_delta
	Usage *struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Message *struct {
		Usage *struct {
			InputTokens int `json:"input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// ParseStream 解析 SSE 流。事件按 event: + data: 两行发出；结束事件 message_stop。
// usage 分散在 message_start（input）与 message_delta（output）里；
// 工具调用的 name 在 content_block_start、参数经 input_json_delta 分段拼接。
func (anthropicAdapter) ParseStream(resp *http.Response, onChunk func(Chunk)) (Response, error) {
	defer func() { _ = resp.Body.Close() }()
	var r Response
	toolAcc := map[int]*toolAccum{}
	var toolOrder []int
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		var ev anthropicSSEEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			return r, &ProtocolError{Kind: "parse", Detail: fmt.Sprintf("SSE 事件不是合法 JSON: %v", err)}
		}
		if ev.Error != nil {
			return r, &ProtocolError{Kind: "protocol", Detail: ev.Error.Message}
		}
		switch ev.Type {
		case "message_start":
			if ev.Message != nil && ev.Message.Usage != nil {
				r.Usage.Prompt = ev.Message.Usage.InputTokens
			}
		case "content_block_start":
			if ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use" && ev.Index != nil {
				toolAcc[*ev.Index] = &toolAccum{Name: ev.ContentBlock.Name}
				toolOrder = append(toolOrder, *ev.Index)
			}
		case "content_block_delta":
			if ev.Delta == nil {
				continue
			}
			switch ev.Delta.Type {
			case "text_delta":
				r.Content += ev.Delta.Text
				onChunk(Chunk{Delta: ev.Delta.Text})
			case "thinking_delta":
				r.ReasoningContent += ev.Delta.Thinking
				onChunk(Chunk{Reasoning: ev.Delta.Thinking})
			case "input_json_delta":
				// 参数片段必须带 index 才能归位；文本/思考增量不依赖它
				if ev.Index != nil {
					if acc, ok := toolAcc[*ev.Index]; ok {
						acc.Args.WriteString(ev.Delta.PartialJSON)
					}
				}
			}
		case "message_delta":
			if ev.Usage != nil {
				r.Usage.Completion += ev.Usage.OutputTokens
			}
		case "message_stop":
			onChunk(Chunk{Done: true})
		}
	}
	if err := sc.Err(); err != nil {
		return r, &ProtocolError{Kind: "parse", Detail: fmt.Sprintf("读取 SSE 流失败: %v", err)}
	}
	for _, idx := range toolOrder {
		acc := toolAcc[idx]
		r.ToolCalls = append(r.ToolCalls, ToolCall{
			Name: acc.Name,
			Args: json.RawMessage(acc.Args.String()),
		})
	}
	return r, nil
}
