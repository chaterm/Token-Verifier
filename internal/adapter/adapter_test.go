package adapter

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeEndpoint 记录收到的请求并返回可编程响应。
type fakeEndpoint struct {
	t        *testing.T
	lastBody string
	lastHdr  http.Header
	handler  func(w http.ResponseWriter, r *http.Request)
}

func (f *fakeEndpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.lastBody = string(body)
	f.lastHdr = r.Header.Clone()
	if f.handler != nil {
		f.handler(w, r)
		return
	}
	w.WriteHeader(200)
	w.Write([]byte(`{"choices":[{"message":{"content":"42"},"finish_reason":"stop"}],"usage":{"prompt_tokens":128,"completion_tokens":3}}`))
}

func newServer(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *fakeEndpoint) {
	t.Helper()
	f := &fakeEndpoint{t: t, handler: h}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return srv, f
}

func TestOpenAIRenderOnce(t *testing.T) {
	srv, f := newServer(t, nil)
	mt := 16
	tmp := 1.0
	lr := LogicalRequest{Model: "m", Prompt: "p", MaxTokens: &mt, Temperature: &tmp, Stream: false}
	req, err := openaiChatAdapter{}.Render(srv.URL, "sk-abc", lr, ThinkingBlock{})
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	var body map[string]any
	if err := json.Unmarshal([]byte(f.lastBody), &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "m" {
		t.Errorf("model = %v", body["model"])
	}
	if body["stream"] != false {
		t.Errorf("stream = %v", body["stream"])
	}
	if body["temperature"] != 1.0 {
		t.Errorf("temperature = %v", body["temperature"])
	}
	if body["max_tokens"] != float64(16) {
		t.Errorf("max_tokens = %v", body["max_tokens"])
	}
	msgs := body["messages"].([]any)
	m0 := msgs[0].(map[string]any)
	if m0["role"] != "user" || m0["content"] != "p" {
		t.Errorf("messages = %v", msgs)
	}
	if got := f.lastHdr.Get("Authorization"); got != "Bearer sk-abc" {
		t.Errorf("Authorization = %q", got)
	}
	if got := f.lastHdr.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q", got)
	}
	// 请求路径是 /chat/completions
	if !strings.HasSuffix(req.URL.Path, "/chat/completions") {
		t.Errorf("path = %s", req.URL.Path)
	}
}

func TestOpenAIOverridesAndPlaceholder(t *testing.T) {
	srv, f := newServer(t, nil)
	mt := 16
	lr := LogicalRequest{Model: "m", Prompt: "p", MaxTokens: &mt, Stream: true}
	tb := ThinkingBlock{BodyOverrides: map[string]any{
		"reasoning_effort": "high",
		"extra":            "x=${thinking_effort}",
	}}
	req, _ := openaiChatAdapter{}.Render(srv.URL, "", lr, tb)
	_, _ = http.DefaultClient.Do(req)

	var body map[string]any
	_ = json.Unmarshal([]byte(f.lastBody), &body)
	if body["reasoning_effort"] != "high" {
		t.Errorf("reasoning_effort = %v, want high", body["reasoning_effort"])
	}
	// 文本插值场景由 renderBody 之上的 expandPlaceholders 单测覆盖
	_ = body["extra"]
}

func TestAnthropicRenderHeaders(t *testing.T) {
	srv, f := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"content":[{"type":"text","text":"42"}],"usage":{"input_tokens":100,"output_tokens":3}}`))
	})
	mt := 16
	lr := LogicalRequest{Model: "m", Prompt: "p", MaxTokens: &mt, Stream: false}
	req, err := anthropicAdapter{}.Render(srv.URL, "sk-ant", lr, ThinkingBlock{})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("POST 到 /chat/completions 返回 %d，测试假端点只挂了根路径", resp.StatusCode)
	}
	r, err := anthropicAdapter{}.ParseOnce(resp)
	if err != nil {
		t.Fatal(err)
	}
	if r.Content != "42" {
		t.Errorf("content = %q", r.Content)
	}
	if r.Usage.Prompt != 100 || r.Usage.Completion != 3 {
		t.Errorf("usage = %+v", r.Usage)
	}
	if got := f.lastHdr.Get("x-api-key"); got != "sk-ant" {
		t.Errorf("x-api-key = %q", got)
	}
	if got := f.lastHdr.Get("anthropic-version"); got == "" {
		t.Errorf("缺少 anthropic-version 头")
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(f.lastBody), &body)
	if body["max_tokens"] != float64(16) {
		t.Errorf("anthropic 必须有 max_tokens，得到 %v", body["max_tokens"])
	}
	if !strings.HasSuffix(req.URL.Path, "/messages") {
		t.Errorf("path = %s", req.URL.Path)
	}
}

func TestOpenAIStream(t *testing.T) {
	srv, f := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"4\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"2\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":128,\"completion_tokens\":3}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	})
	mt := 16
	lr := LogicalRequest{Model: "m", Prompt: "p", MaxTokens: &mt, Stream: true}
	req, _ := openaiChatAdapter{}.Render(srv.URL, "", lr, ThinkingBlock{})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var deltas []string
	var done bool
	r, err := openaiChatAdapter{}.ParseStream(resp, func(c Chunk) {
		if c.Delta != "" {
			deltas = append(deltas, c.Delta)
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
	if !done {
		t.Error("应收到 [DONE]")
	}
	if r.Content != "42" || r.Usage.Completion != 3 || r.Usage.Prompt != 128 {
		t.Errorf("r = %+v", r)
	}
	_ = f
}

func TestAnthropicStream(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":100}}}\n\n")
		io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"4\"}}\n\n")
		io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"2\"}}\n\n")
		io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":3}}\n\n")
		io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	})
	mt := 16
	lr := LogicalRequest{Model: "m", Prompt: "p", MaxTokens: &mt, Stream: true}
	req, _ := anthropicAdapter{}.Render(srv.URL, "k", lr, ThinkingBlock{})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var deltas []string
	r, err := anthropicAdapter{}.ParseStream(resp, func(c Chunk) {
		if c.Delta != "" {
			deltas = append(deltas, c.Delta)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(deltas, "") != "42" {
		t.Errorf("deltas = %v", deltas)
	}
	if r.Content != "42" || r.Usage.Prompt != 100 || r.Usage.Completion != 3 {
		t.Errorf("r = %+v", r)
	}
}

func TestParseOnceProtocolError(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"error":{"message":"bad request body"}}`))
	})
	resp, _ := http.Get(srv.URL)
	_, err := openaiChatAdapter{}.ParseOnce(resp)
	if err == nil {
		t.Fatal("error 响应应报错")
	}
	var pe *ProtocolError
	if !errorsAs(err, &pe) || pe.Kind != "protocol" {
		t.Errorf("err = %v, want protocol error", err)
	}
}

func TestParseOnceMalformedJSON(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`not json`))
	})
	resp, _ := http.Get(srv.URL)
	_, err := openaiChatAdapter{}.ParseOnce(resp)
	var pe *ProtocolError
	if !errorsAs(err, &pe) || pe.Kind != "parse" {
		t.Errorf("err = %v, want parse error", err)
	}
}

func errorsAs(err error, target any) bool {
	type causer interface{ Unwrap() error }
	for err != nil {
		if e, ok := err.(*ProtocolError); ok {
			*target.(**ProtocolError) = e
			return true
		}
		c, ok := err.(causer)
		if !ok {
			return false
		}
		err = c.Unwrap()
	}
	return false
}

func TestExpandPlaceholders(t *testing.T) {
	// 类型保留：整值替换
	if got := ExpandPlaceholders("${thinking_effort}", 16384); got != 16384 {
		t.Errorf("整值替换 = %v (%T), want 16384 (int)", got, got)
	}
	// 嵌字符串：文本插值
	if got := ExpandPlaceholders("budget=${thinking_effort}", 16384); got != "budget=16384" {
		t.Errorf("文本插值 = %v", got)
	}
	// 嵌套深度：map 内数组内 map
	got := ExpandPlaceholders(map[string]any{
		"thinking": map[string]any{
			"budget_tokens": "${thinking_effort}",
			"levels":        []any{"${thinking_effort}", "x=${thinking_effort}"},
		},
	}, 4096)
	th := got.(map[string]any)["thinking"].(map[string]any)
	if th["budget_tokens"] != 4096 {
		t.Errorf("嵌套整值替换 = %v", th["budget_tokens"])
	}
	if th["levels"].([]any)[0] != 4096 || th["levels"].([]any)[1] != "x=4096" {
		t.Errorf("数组内替换 = %v", th["levels"])
	}
	// 无映射时保留原样（校验应已拦住，这是兜底）
	if got := ExpandPlaceholders("${thinking_effort}", nil); got != "${thinking_effort}" {
		t.Errorf("无映射时应原样保留 = %v", got)
	}
}

func TestRenderBodyTempPath(t *testing.T) {
	// temperature_field 点路径：generation_config.temperature
	body, err := renderBody(map[string]any{"model": "m"}, nil, "generation_config.temperature", f64p(0.5))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	gc := m["generation_config"].(map[string]any)
	if gc["temperature"] != 0.5 {
		t.Errorf("generation_config.temperature = %v", gc["temperature"])
	}
}

func f64p(f float64) *float64 { return &f }

// —— system prompt（输出契约强约束）——

func TestOpenAISystemMessage(t *testing.T) {
	srv, f := newServer(t, nil)
	lr := LogicalRequest{Model: "m", Prompt: "p", System: "只回答 JSON"}
	req, _ := openaiChatAdapter{}.Render(srv.URL, "k", lr, ThinkingBlock{})
	_, _ = http.DefaultClient.Do(req)

	var body map[string]any
	json.Unmarshal([]byte(f.lastBody), &body)
	msgs := body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages 长度 = %d, want 2（system + user）", len(msgs))
	}
	m0 := msgs[0].(map[string]any)
	m1 := msgs[1].(map[string]any)
	if m0["role"] != "system" || m0["content"] != "只回答 JSON" {
		t.Errorf("messages[0] = %v, want system", m0)
	}
	if m1["role"] != "user" || m1["content"] != "p" {
		t.Errorf("messages[1] = %v, want user", m1)
	}
}

func TestOpenAINoSystemWhenEmpty(t *testing.T) {
	srv, f := newServer(t, nil)
	lr := LogicalRequest{Model: "m", Prompt: "p"}
	req, _ := openaiChatAdapter{}.Render(srv.URL, "k", lr, ThinkingBlock{})
	_, _ = http.DefaultClient.Do(req)

	var body map[string]any
	json.Unmarshal([]byte(f.lastBody), &body)
	msgs := body["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["role"] != "user" {
		t.Errorf("System 为空不应发 system 消息: %v", msgs)
	}
}

func TestAnthropicSystemField(t *testing.T) {
	srv, f := newServer(t, nil)
	lr := LogicalRequest{Model: "m", Prompt: "p", System: "只回答 JSON"}
	req, _ := anthropicAdapter{}.Render(srv.URL, "k", lr, ThinkingBlock{})
	_, _ = http.DefaultClient.Do(req)

	var body map[string]any
	json.Unmarshal([]byte(f.lastBody), &body)
	if body["system"] != "只回答 JSON" {
		t.Errorf("system = %v, want 只回答 JSON", body["system"])
	}
	msgs := body["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["role"] != "user" {
		t.Errorf("anthropic 的 system 是顶层字段，messages 应只有 user: %v", msgs)
	}
}

func TestAnthropicNoSystemWhenEmpty(t *testing.T) {
	srv, f := newServer(t, nil)
	lr := LogicalRequest{Model: "m", Prompt: "p"}
	req, _ := anthropicAdapter{}.Render(srv.URL, "k", lr, ThinkingBlock{})
	_, _ = http.DefaultClient.Do(req)

	var body map[string]any
	json.Unmarshal([]byte(f.lastBody), &body)
	if _, ok := body["system"]; ok {
		t.Errorf("System 为空不应发送 system 字段: %v", body["system"])
	}
}

func TestRenderBodySystemReserved(t *testing.T) {
	// system 是工具掌控的保留字段，body_overrides 不许覆盖（anthropic 顶层字段形态）
	skeleton := map[string]any{"model": "m", "system": "契约文本"}
	overrides := map[string]any{"system": "被注入的"}
	out, err := renderBody(skeleton, overrides, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	json.Unmarshal(out, &body)
	if body["system"] != "契约文本" {
		t.Errorf("system 被 overrides 覆盖: %v", body["system"])
	}
}

func TestRenderBodyUsesConfigReservedFields(t *testing.T) {
	// overrides 里的保留字段被忽略（与 config.ReservedFields 同一来源）
	body, err := renderBody(map[string]any{"a": 1}, map[string]any{"model": "evil", "system": "x", "ok": true}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if _, has := m["model"]; has {
		t.Error("model 是保留字段，不应被 override 写入")
	}
	if _, has := m["system"]; has {
		t.Error("system 是保留字段，不应被 override 写入")
	}
	if v, _ := m["ok"].(bool); !v {
		t.Error("非保留字段 ok 应被写入")
	}
}
