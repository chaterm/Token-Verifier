package adapter

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// 本文件覆盖 openai-responses 适配器与两个现有适配器的
// reasoning tokens / tool_calls 补充解析。

// —— openai-responses ——

func TestResponsesRenderOnce(t *testing.T) {
	srv, f := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"42"}]},{"type":"reasoning","summary":[{"text":"想了很久"}]}],"usage":{"input_tokens":100,"output_tokens":50,"output_tokens_details":{"reasoning_tokens":30},"input_tokens_details":{"cached_tokens":20}}}`))
	})
	mt := 2048
	tmp := 1.0
	lr := LogicalRequest{Model: "m", System: "契约", Prompt: "p", MaxTokens: &mt, Temperature: &tmp, Stream: false}
	req, err := openaiResponsesAdapter{}.Render(srv.URL, "sk-r", lr, ThinkingBlock{})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r, err := openaiResponsesAdapter{}.ParseOnce(resp)
	if err != nil {
		t.Fatal(err)
	}

	// 请求体形态
	var body map[string]any
	if err := json.Unmarshal([]byte(f.lastBody), &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "m" || body["stream"] != false {
		t.Errorf("body = %v", body)
	}
	if body["max_output_tokens"] != float64(2048) {
		t.Errorf("max_output_tokens = %v", body["max_output_tokens"])
	}
	if body["temperature"] != 1.0 {
		t.Errorf("temperature = %v", body["temperature"])
	}
	// 默认开启 reasoning（think-effort 探针需要服务端上报思考 token）
	rsn, _ := body["reasoning"].(map[string]any)
	if rsn == nil || rsn["effort"] != "high" || rsn["summary"] != "auto" {
		t.Errorf("reasoning 默认块 = %v, want {effort:high summary:auto}", body["reasoning"])
	}
	// input：system + user 两条，content 是 input_text 数组
	input := body["input"].([]any)
	if len(input) != 2 {
		t.Fatalf("input = %v, want 2 条", input)
	}
	m0 := input[0].(map[string]any)
	if m0["role"] != "system" {
		t.Errorf("input[0].role = %v", m0["role"])
	}
	c0 := m0["content"].([]any)[0].(map[string]any)
	if c0["type"] != "input_text" || c0["text"] != "契约" {
		t.Errorf("input[0].content = %v", m0["content"])
	}
	if got := f.lastHdr.Get("Authorization"); got != "Bearer sk-r" {
		t.Errorf("Authorization = %q", got)
	}

	// 响应归一：content / reasoning / usage（含 reasoning 与 cached 细分）
	if r.Content != "42" {
		t.Errorf("Content = %q", r.Content)
	}
	if r.ReasoningContent != "想了很久" {
		t.Errorf("ReasoningContent = %q", r.ReasoningContent)
	}
	if r.FinishReason != "completed" {
		t.Errorf("FinishReason = %q", r.FinishReason)
	}
	if r.Usage.Prompt != 100 || r.Usage.Completion != 50 || r.Usage.Reasoning != 30 || r.Usage.Cached != 20 {
		t.Errorf("Usage = %+v", r.Usage)
	}
}

func TestResponsesReasoningOverride(t *testing.T) {
	// thinking 块注入覆盖默认 reasoning（enabled effort_map 展开后的形态）
	srv, f := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"completed","output":[],"usage":{}}`))
	})
	lr := LogicalRequest{Model: "m", Prompt: "p"}
	tb := ThinkingBlock{BodyOverrides: map[string]any{
		"reasoning": map[string]any{"effort": "low"},
	}}
	req, err := openaiResponsesAdapter{}.Render(srv.URL, "k", lr, tb)
	if err != nil {
		t.Fatal(err)
	}
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()

	var body map[string]any
	json.Unmarshal([]byte(f.lastBody), &body)
	rsn := body["reasoning"].(map[string]any)
	// renderBody 对骨架的 overrides 是浅合并（整键替换）：
	// 覆盖 reasoning 时须给出完整块（全局与 thinking 块之间的深合并
	// 在 collect.resolveInjection 完成，适配器不做二次深合并）
	if rsn["effort"] != "low" || len(rsn) != 1 {
		t.Errorf("reasoning = %v, want {effort:low}（整键替换默认块）", rsn)
	}
}

func TestResponsesRenderToolsReserved(t *testing.T) {
	srv, f := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"completed","output":[],"usage":{}}`))
	})
	lr := LogicalRequest{Model: "m", Prompt: "p", Tools: []ToolDef{
		{Name: "get_weather", Description: "查天气", Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"city": map[string]any{"type": "string"}},
			"required":   []any{"city"},
		}},
	}}
	// body_overrides 试图覆盖 tools（保留字段）→ 被忽略
	tb := ThinkingBlock{BodyOverrides: map[string]any{"tools": []any{"evil"}}}
	req, err := openaiResponsesAdapter{}.Render(srv.URL, "k", lr, tb)
	if err != nil {
		t.Fatal(err)
	}
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()

	var body map[string]any
	json.Unmarshal([]byte(f.lastBody), &body)
	tools := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v, want 1 个", tools)
	}
	t0 := tools[0].(map[string]any)
	// Responses 形态：扁平 function 对象
	if t0["type"] != "function" || t0["name"] != "get_weather" {
		t.Errorf("tools[0] = %v", t0)
	}
	if _, has := t0["function"]; has {
		t.Error("Responses 工具是扁平形态，不应有 function 包装")
	}
}

func TestResponsesParseToolCalls(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"completed","output":[{"type":"function_call","name":"get_weather","arguments":"{\"city\":\"北京\"}"},{"type":"function_call","name":"get_weather","arguments":"{\"city\":\"广州\"}"}],"usage":{"input_tokens":10,"output_tokens":8}}`))
	})
	lr := LogicalRequest{Model: "m", Prompt: "p"}
	req, _ := openaiResponsesAdapter{}.Render(srv.URL, "k", lr, ThinkingBlock{})
	resp, _ := http.DefaultClient.Do(req)
	r, err := openaiResponsesAdapter{}.ParseOnce(resp)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.ToolCalls) != 2 {
		t.Fatalf("ToolCalls = %+v, want 2", r.ToolCalls)
	}
	if r.ToolCalls[0].Name != "get_weather" || string(r.ToolCalls[0].Args) != `{"city":"北京"}` {
		t.Errorf("ToolCalls[0] = %+v", r.ToolCalls[0])
	}
	if r.ToolCalls[1].Name != "get_weather" {
		t.Errorf("ToolCalls[1] = %+v", r.ToolCalls[1])
	}
}

func TestResponsesStream(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"4\"}\n\n")
		io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"2\"}\n\n")
		io.WriteString(w, "event: response.reasoning_summary_text.delta\ndata: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"嗯\"}\n\n")
		io.WriteString(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"name\":\"get_weather\",\"arguments\":\"{\\\"city\\\":\\\"北京\\\"}\"}}\n\n")
		io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":100,\"output_tokens\":9,\"output_tokens_details\":{\"reasoning_tokens\":4}}}}\n\n")
	})
	lr := LogicalRequest{Model: "m", Prompt: "p", Stream: true}
	req, _ := openaiResponsesAdapter{}.Render(srv.URL, "k", lr, ThinkingBlock{})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var deltas, reasoning []string
	done := false
	r, err := openaiResponsesAdapter{}.ParseStream(resp, func(c Chunk) {
		if c.Delta != "" {
			deltas = append(deltas, c.Delta)
		}
		if c.Reasoning != "" {
			reasoning = append(reasoning, c.Reasoning)
		}
		if c.Done {
			done = true
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(deltas, "") != "42" {
		t.Errorf("deltas = %v", deltas)
	}
	if strings.Join(reasoning, "") != "嗯" {
		t.Errorf("reasoning chunks = %v", reasoning)
	}
	if !done {
		t.Error("response.completed 应触发 Done chunk")
	}
	if r.Content != "42" || r.ReasoningContent != "嗯" {
		t.Errorf("r = %+v", r)
	}
	if r.Usage.Prompt != 100 || r.Usage.Completion != 9 || r.Usage.Reasoning != 4 {
		t.Errorf("Usage = %+v", r.Usage)
	}
	if len(r.ToolCalls) != 1 || r.ToolCalls[0].Name != "get_weather" {
		t.Errorf("ToolCalls = %+v", r.ToolCalls)
	}
	if r.FinishReason != "completed" {
		t.Errorf("FinishReason = %q", r.FinishReason)
	}
}

func TestResponsesRegistry(t *testing.T) {
	a, ok := Get("openai-responses")
	if !ok || a.ID() != "openai-responses" {
		t.Errorf("openai-responses 未注册: %v %v", a, ok)
	}
}

// —— 现有适配器补解析 ——

func TestOpenAIChatReasoningAndTools(t *testing.T) {
	// 非流式：reasoning_content 与 tool_calls 解析 + usage 细分归一
	srv, f := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"","reasoning_content":"想一想","tool_calls":[{"function":{"name":"get_weather","arguments":"{\"city\":\"北京\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":20,"completion_tokens":8,"prompt_tokens_details":{"cached_tokens":5},"completion_tokens_details":{"reasoning_tokens":6}}}`))
	})
	lr := LogicalRequest{Model: "m", Prompt: "p", Tools: []ToolDef{
		{Name: "get_weather", Description: "查天气", Parameters: map[string]any{"type": "object"}},
	}}
	req, _ := openaiChatAdapter{}.Render(srv.URL, "k", lr, ThinkingBlock{})
	resp, _ := http.DefaultClient.Do(req)
	r, err := openaiChatAdapter{}.ParseOnce(resp)
	if err != nil {
		t.Fatal(err)
	}
	if r.ReasoningContent != "想一想" {
		t.Errorf("ReasoningContent = %q", r.ReasoningContent)
	}
	if len(r.ToolCalls) != 1 || r.ToolCalls[0].Name != "get_weather" || string(r.ToolCalls[0].Args) != `{"city":"北京"}` {
		t.Errorf("ToolCalls = %+v", r.ToolCalls)
	}
	if r.Usage.Reasoning != 6 || r.Usage.Cached != 5 {
		t.Errorf("Usage = %+v", r.Usage)
	}
	// Chat Completions 工具形态：type:function 包装
	var body map[string]any
	json.Unmarshal([]byte(f.lastBody), &body)
	t0 := body["tools"].([]any)[0].(map[string]any)
	fn, ok := t0["function"].(map[string]any)
	if t0["type"] != "function" || !ok || fn["name"] != "get_weather" {
		t.Errorf("tools[0] = %v", t0)
	}
}

func TestOpenAIChatStreamToolCallAccumulate(t *testing.T) {
	// 流式：tool_calls 分片（name 首片 + arguments 多片）按 index 累积
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"想\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"ci\"}}]}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"ty\\\":\\\"北京\\\"}\"}}]}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":1,\"function\":{\"name\":\"get_forecast\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n")
		io.WriteString(w, "data: {\"usage\":{\"prompt_tokens\":30,\"completion_tokens\":12,\"completion_tokens_details\":{\"reasoning_tokens\":7}}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	})
	lr := LogicalRequest{Model: "m", Prompt: "p", Stream: true}
	req, _ := openaiChatAdapter{}.Render(srv.URL, "k", lr, ThinkingBlock{})
	resp, _ := http.DefaultClient.Do(req)
	var reasoning []string
	r, err := openaiChatAdapter{}.ParseStream(resp, func(c Chunk) {
		if c.Reasoning != "" {
			reasoning = append(reasoning, c.Reasoning)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(reasoning, "") != "想" {
		t.Errorf("reasoning chunks = %v", reasoning)
	}
	if r.ReasoningContent != "想" {
		t.Errorf("ReasoningContent = %q", r.ReasoningContent)
	}
	if len(r.ToolCalls) != 2 {
		t.Fatalf("ToolCalls = %+v, want 2", r.ToolCalls)
	}
	if r.ToolCalls[0].Name != "get_weather" || string(r.ToolCalls[0].Args) != `{"city":"北京"}` {
		t.Errorf("ToolCalls[0] = %+v（arguments 分片应拼接）", r.ToolCalls[0])
	}
	if r.ToolCalls[1].Name != "get_forecast" || string(r.ToolCalls[1].Args) != "{}" {
		t.Errorf("ToolCalls[1] = %+v", r.ToolCalls[1])
	}
	if r.FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q", r.FinishReason)
	}
	if r.Usage.Reasoning != 7 {
		t.Errorf("Usage = %+v", r.Usage)
	}
}

func TestAnthropicToolsAndThinkingStream(t *testing.T) {
	// 非流式：tools 渲染进 input_schema；tool_use content block 解析
	srv, f := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"content":[{"type":"tool_use","name":"get_weather","input":{"city":"北京"}}],"usage":{"input_tokens":30,"output_tokens":9}}`))
	})
	mt := 512
	lr := LogicalRequest{Model: "m", Prompt: "p", MaxTokens: &mt, Tools: []ToolDef{
		{Name: "get_weather", Description: "查天气", Parameters: map[string]any{"type": "object"}},
	}}
	req, _ := anthropicAdapter{}.Render(srv.URL, "k", lr, ThinkingBlock{})
	resp, _ := http.DefaultClient.Do(req)
	r, err := anthropicAdapter{}.ParseOnce(resp)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	json.Unmarshal([]byte(f.lastBody), &body)
	t0 := body["tools"].([]any)[0].(map[string]any)
	if t0["name"] != "get_weather" || t0["input_schema"] == nil {
		t.Errorf("tools[0] = %v（parameters 应进 input_schema）", t0)
	}
	if len(r.ToolCalls) != 1 || r.ToolCalls[0].Name != "get_weather" || string(r.ToolCalls[0].Args) != `{"city":"北京"}` {
		t.Errorf("ToolCalls = %+v", r.ToolCalls)
	}

	// 流式：thinking_delta 与 input_json_delta（含 index）
	srv2, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":30}}}\n\n")
		io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"name\":\"get_weather\"}}\n\n")
		io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"city\\\":\"}}\n\n")
		io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"\\\"北京\\\"}\"}}\n\n")
		io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"嗯\"}}\n\n")
		io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":9}}\n\n")
		io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	})
	req2, _ := anthropicAdapter{}.Render(srv2.URL, "k", LogicalRequest{Model: "m", Prompt: "p", Stream: true}, ThinkingBlock{})
	resp2, _ := http.DefaultClient.Do(req2)
	r2, err := anthropicAdapter{}.ParseStream(resp2, func(c Chunk) {})
	if err != nil {
		t.Fatal(err)
	}
	if r2.ReasoningContent != "嗯" {
		t.Errorf("ReasoningContent = %q", r2.ReasoningContent)
	}
	if len(r2.ToolCalls) != 1 || r2.ToolCalls[0].Name != "get_weather" || string(r2.ToolCalls[0].Args) != `{"city":"北京"}` {
		t.Errorf("ToolCalls = %+v", r2.ToolCalls)
	}
}

func TestReasoningUnit(t *testing.T) {
	// 单位由协议静态决定：anthropic 无独立思考 token 字段 → chars；
	// openai 两协议上报 reasoning_tokens → tokens；未知协议按 tokens 兜底
	if got := ReasoningUnit("anthropic-messages"); got != "chars" {
		t.Errorf("anthropic-messages = %s, want chars", got)
	}
	if got := ReasoningUnit("openai-chat"); got != "tokens" {
		t.Errorf("openai-chat = %s, want tokens", got)
	}
	if got := ReasoningUnit("openai-responses"); got != "tokens" {
		t.Errorf("openai-responses = %s, want tokens", got)
	}
	if got := ReasoningUnit("some-future-proto"); got != "tokens" {
		t.Errorf("未知协议 = %s, want tokens（兜底）", got)
	}
}

func TestAnthropicParseOnceThinkingBlock(t *testing.T) {
	// 非流式 thinking 块进 ReasoningContent（chars 单位的观测通道）
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"content":[{"type":"thinking","thinking":"先想两步"},{"type":"text","text":"答案"}],"usage":{"input_tokens":10,"output_tokens":8}}`))
	})
	req, _ := anthropicAdapter{}.Render(srv.URL, "k", LogicalRequest{Model: "m", Prompt: "p"}, ThinkingBlock{})
	resp, _ := http.DefaultClient.Do(req)
	r, err := anthropicAdapter{}.ParseOnce(resp)
	if err != nil {
		t.Fatal(err)
	}
	if r.ReasoningContent != "先想两步" {
		t.Errorf("ReasoningContent = %q, want 先想两步", r.ReasoningContent)
	}
	if r.Content != "答案" {
		t.Errorf("Content = %q", r.Content)
	}
}
