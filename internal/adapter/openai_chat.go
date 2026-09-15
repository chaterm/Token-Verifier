package adapter

// openaiChat 适配器：Chat Completions 风格（openai-chat）。
// 端点形态近似 POST {base}/chat/completions，用法见 docs/SPEC-CONFIG.md。
// 工具不内置厂商字段名表（SPEC-CONFIG 开头）：这里按协议常见形态实现，
// 具体字段以对接端点当时文档为准，扩展字段通过 body_overrides 注入。

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// openaiChatAdapter 对应协议 id "openai-chat"。
type openaiChatAdapter struct{}

const openaiDefaultPath = "/chat/completions"

func (openaiChatAdapter) ID() string { return "openai-chat" }

type openaiMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// openaiResponse 非流式响应结构。
type openaiResponse struct {
	Choices []struct {
		Message struct {
			Content   string `json:"content"`
			Reasoning string `json:"reasoning_content"`
			ToolCalls []struct {
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *openaiUsage `json:"usage"`
	Error errorField   `json:"error"`
}

// openaiUsage Chat Completions 的 usage 形态（含思考 token 细分）。
type openaiUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// normalize 归一为统一 Usage。
func (u *openaiUsage) normalize() Usage {
	out := Usage{Prompt: u.PromptTokens, Completion: u.CompletionTokens}
	if u.PromptTokensDetails != nil {
		out.Cached = u.PromptTokensDetails.CachedTokens
	}
	if u.CompletionTokensDetails != nil {
		out.Reasoning = u.CompletionTokensDetails.ReasoningTokens
	}
	return out
}

// Render 构造 Chat Completions 请求。消息体由工具掌控，保留字段不被覆盖。
func (a openaiChatAdapter) Render(baseURL, apiKey string, lr LogicalRequest, tb ThinkingBlock) (*http.Request, error) {
	msgs := []openaiMsg{}
	if lr.System != "" {
		msgs = append(msgs, openaiMsg{Role: "system", Content: lr.System})
	}
	msgs = append(msgs, openaiMsg{Role: "user", Content: lr.Padding + lr.Prompt})
	skeleton := map[string]any{
		"model":    lr.Model,
		"messages": msgs,
		"stream":   lr.Stream,
	}
	if lr.Stream {
		// Chat Completions 流式默认不上报 usage，不显式开启会让所有流式
		// 观测拿不到 token 数。放在骨架里（非保留字段），端点不识别时可
		// 经 body_overrides 覆盖。
		skeleton["stream_options"] = map[string]any{"include_usage": true}
	}
	if lr.MaxTokens != nil {
		skeleton["max_tokens"] = *lr.MaxTokens
	}
	if lr.TopP != nil {
		skeleton["top_p"] = *lr.TopP
	}
	if len(lr.Tools) > 0 {
		skeleton["tools"] = openaiToolsPayload(lr.Tools)
	}
	payload, err := renderBody(skeleton, tb.BodyOverrides, lr.TemperatureField, lr.Temperature)
	if err != nil {
		return nil, fmt.Errorf("序列化请求体失败: %w", err)
	}
	path := lr.Path
	if path == "" {
		path = openaiDefaultPath
	}
	url := joinURL(baseURL, path)
	return postJSON(url, apiKey, "application/json", payload)
}

// openaiToolsPayload 工具集的 Chat Completions 形态：type:function 包装。
func openaiToolsPayload(tools []ToolDef) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  t.Parameters,
			},
		})
	}
	return out
}

// ParseOnce 解析非流式响应。
func (openaiChatAdapter) ParseOnce(resp *http.Response) (Response, error) {
	body, err := readAll(resp)
	if err != nil {
		return Response{}, fmt.Errorf("读取响应体失败: %w", err)
	}
	var out openaiResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return Response{}, &ProtocolError{Kind: "parse", Detail: fmt.Sprintf("响应不是合法 JSON: %v", err)}
	}
	if out.Error.IsError() {
		return Response{}, &ProtocolError{Kind: "protocol", Detail: out.Error.Message}
	}
	var r Response
	if len(out.Choices) > 0 {
		choice := out.Choices[0]
		r.Content = choice.Message.Content
		r.ReasoningContent = choice.Message.Reasoning
		r.FinishReason = choice.FinishReason
		for _, tc := range choice.Message.ToolCalls {
			r.ToolCalls = append(r.ToolCalls, ToolCall{
				Name: tc.Function.Name,
				Args: json.RawMessage(tc.Function.Arguments),
			})
		}
	}
	if out.Usage != nil {
		r.Usage = out.Usage.normalize()
	}
	return r, nil
}

// openaiSSEChunk 流式分片。
type openaiSSEChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			Reasoning string `json:"reasoning_content"`
			ToolCalls []struct {
				Index    int `json:"index"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *openaiUsage `json:"usage"`
	Error errorField   `json:"error"`
}

// ParseStream 解析 SSE 流。每条 data: 一行是一个事件；data: [DONE] 结束。
func (openaiChatAdapter) ParseStream(resp *http.Response, onChunk func(Chunk)) (Response, error) {
	defer func() { _ = resp.Body.Close() }()
	var r Response
	// 工具调用按 delta.index 累积：name 只随首个片段到达，arguments 分段拼接
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
		if payload == "[DONE]" {
			onChunk(Chunk{Done: true})
			break
		}
		var c openaiSSEChunk
		if err := json.Unmarshal([]byte(payload), &c); err != nil {
			return r, &ProtocolError{Kind: "parse", Detail: fmt.Sprintf("SSE 事件不是合法 JSON: %v", err)}
		}
		if c.Error.IsError() {
			return r, &ProtocolError{Kind: "protocol", Detail: c.Error.Message}
		}
		ch := Chunk{}
		if len(c.Choices) > 0 {
			choice := c.Choices[0]
			ch.Delta = choice.Delta.Content
			ch.Reasoning = choice.Delta.Reasoning
			if choice.FinishReason != "" {
				r.FinishReason = choice.FinishReason
			}
			for _, tc := range choice.Delta.ToolCalls {
				acc, ok := toolAcc[tc.Index]
				if !ok {
					acc = &toolAccum{}
					toolAcc[tc.Index] = acc
					toolOrder = append(toolOrder, tc.Index)
				}
				if tc.Function.Name != "" {
					acc.Name = tc.Function.Name
				}
				acc.Args.WriteString(tc.Function.Arguments)
			}
		}
		if c.Usage != nil {
			u := c.Usage.normalize()
			r.Usage = u
			ch.Usage = &u
		}
		r.Content += ch.Delta
		r.ReasoningContent += ch.Reasoning
		if ch.Done || ch.Delta != "" || ch.Reasoning != "" || ch.Usage != nil {
			onChunk(ch)
		}
	}
	if err := sc.Err(); err != nil {
		return r, &ProtocolError{Kind: "parse", Detail: fmt.Sprintf("读取 SSE 流失败: %v", err)}
	}
	// 首次出现顺序即调用顺序（index 缺省时全为 0，退化为单调用）
	for _, idx := range toolOrder {
		acc := toolAcc[idx]
		r.ToolCalls = append(r.ToolCalls, ToolCall{
			Name: acc.Name,
			Args: json.RawMessage(acc.Args.String()),
		})
	}
	return r, nil
}

// toolAccum 流式工具调用的累积器。
type toolAccum struct {
	Name string
	Args strings.Builder
}

// joinURL 拼接 base 与路径：保证恰好一个斜杠分隔，容忍 base 带/不带尾斜杠。
func joinURL(base, path string) string {
	b := strings.TrimSuffix(base, "/")
	p := strings.TrimPrefix(path, "/")
	return b + "/" + p
}

// postJSON 发 POST 请求（带可选 Bearer 认证）。
func postJSON(url, apiKey, contentType string, body []byte) (*http.Request, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	return req, nil
}
