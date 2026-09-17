package adapter

// 恶意端点防护的回归测试：
//   - 非流式响应体总量上限（readAll）
//   - 流式响应内容累积上限（三个 ParseStream）
//
// 背景：io.ReadAll 无上限时，恶意端点用 gzip 炸弹/无限流可以把采集机内存
// 吃到爆（实测 30s 涨到 ~10GB）。上限值 = maxResponseBodyBytes（512KiB），
// 对「极短回答 + usage」的探针语义绰绰有余，超长本身就是异常信号。

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestParseOnceRejectsOversizedBody 非流式：响应体超过上限应报 parse 类
// ProtocolError，而不是把整个 body 读进内存。
func TestParseOnceRejectsOversizedBody(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		junk := strings.Repeat("A", 4096)
		for i := 0; i < 200; i++ { // ~800KB > 512KiB
			io.WriteString(w, junk)
		}
	})
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, err = openaiChatAdapter{}.ParseOnce(resp)
	var pe *ProtocolError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want *ProtocolError", err)
	}
	if pe.Kind != "parse" {
		t.Errorf("Kind = %q, want parse", pe.Kind)
	}
	if !strings.Contains(pe.Detail, "上限") {
		t.Errorf("Detail = %q, want 提到上限", pe.Detail)
	}
}

// TestParseOnceAcceptsBodyUnderLimit 上限以内的正常响应不受影响。
func TestParseOnceAcceptsBodyUnderLimit(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		// 一个较大但低于上限的合法响应（content 100KB）
		content := strings.Repeat("x", 100*1024)
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`, content)
	})
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	r, err := openaiChatAdapter{}.ParseOnce(resp)
	if err != nil {
		t.Fatalf("低于上限的响应不应报错: %v", err)
	}
	if len(r.Content) != 100*1024 {
		t.Errorf("content len = %d", len(r.Content))
	}
}

// streamBomb 返回一个 handler：持续输出携带 chunkSize 字节 content 增量的
// SSE 事件，总量远超累积上限。ParseStream 应在越过上限时尽早报错终止。
func TestOpenAIStreamStopsAtAccumLimit(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		payload := strings.Repeat("B", 1024)
		for i := 0; i < 4096; i++ { // 4MB 增量 ≫ 512KiB
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", payload)
		}
		io.WriteString(w, "data: [DONE]\n\n")
	})
	req, _ := openaiChatAdapter{}.Render(srv.URL, "", LogicalRequest{Model: "m", Prompt: "p", Stream: true}, ThinkingBlock{})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, err = openaiChatAdapter{}.ParseStream(resp, func(Chunk) {})
	var pe *ProtocolError
	if !errors.As(err, &pe) || pe.Kind != "parse" {
		t.Fatalf("err = %v, want parse ProtocolError", err)
	}
	if !strings.Contains(pe.Detail, "上限") {
		t.Errorf("Detail = %q, want 提到上限", pe.Detail)
	}
}

func TestAnthropicStreamStopsAtAccumLimit(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		payload := strings.Repeat("B", 1024)
		for i := 0; i < 4096; i++ {
			// 交替塞 text_delta 与 thinking_delta，两个通道都必须计入预算
			if i%2 == 0 {
				fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", payload)
			} else {
				fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":%q}}\n\n", payload)
			}
		}
	})
	req, _ := anthropicAdapter{}.Render(srv.URL, "k", LogicalRequest{Model: "m", Prompt: "p", Stream: true}, ThinkingBlock{})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, err = anthropicAdapter{}.ParseStream(resp, func(Chunk) {})
	var pe *ProtocolError
	if !errors.As(err, &pe) || pe.Kind != "parse" {
		t.Fatalf("err = %v, want parse ProtocolError", err)
	}
}

func TestResponsesStreamStopsAtAccumLimit(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		payload := strings.Repeat("B", 1024)
		for i := 0; i < 4096; i++ {
			fmt.Fprintf(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\n", payload)
		}
	})
	req, _ := openaiResponsesAdapter{}.Render(srv.URL, "", LogicalRequest{Model: "m", Prompt: "p", Stream: true}, ThinkingBlock{})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, err = openaiResponsesAdapter{}.ParseStream(resp, func(Chunk) {})
	var pe *ProtocolError
	if !errors.As(err, &pe) || pe.Kind != "parse" {
		t.Fatalf("err = %v, want parse ProtocolError", err)
	}
}

// TestOpenAIStreamToolArgsCountedInBudget 工具参数分片也计入累积预算：
// 攻击者可以用无限 input_json_delta/arguments 分片撑爆内存。
func TestOpenAIStreamToolArgsCountedInBudget(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		payload := strings.Repeat("C", 1024)
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"f\"}}]}}]}\n\n")
		for i := 0; i < 4096; i++ {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":%q}}]}}]}\n\n", payload)
		}
	})
	req, _ := openaiChatAdapter{}.Render(srv.URL, "", LogicalRequest{Model: "m", Prompt: "p", Stream: true}, ThinkingBlock{})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, err = openaiChatAdapter{}.ParseStream(resp, func(Chunk) {})
	var pe *ProtocolError
	if !errors.As(err, &pe) || pe.Kind != "parse" {
		t.Fatalf("err = %v, want parse ProtocolError", err)
	}
}
