package collect

import (
	"testing"

	"github.com/chaterm/token-verifier/internal/config"
)

func f64p(f float64) *float64 { return &f }

func TestLargestRemainder(t *testing.T) {
	// SPEC-CONFIG §5.1 示例：repeats=20，协议 0.6/0.4 → 12/8
	got := LargestRemainder(20, []float64{0.6, 0.4})
	if got[0] != 12 || got[1] != 8 {
		t.Errorf("20 @ 0.6/0.4 = %v, want [12 8]", got)
	}
	// 协议的 12 再按 transport 0.5/0.5 → 6/6
	got2 := LargestRemainder(12, []float64{0.5, 0.5})
	if got2[0] != 6 || got2[1] != 6 {
		t.Errorf("12 @ 0.5/0.5 = %v, want [6 6]", got2)
	}
	// anthropic 的 8 按 1.0/0.0 → 8/0
	got3 := LargestRemainder(8, []float64{0.0, 1.0})
	if got3[0] != 0 || got3[1] != 8 {
		t.Errorf("8 @ 0/1 = %v, want [0 8]", got3)
	}
	// 平局按下标升序：N=2, 权重相等
	got4 := LargestRemainder(2, []float64{1, 1, 1})
	if got4[0] != 1 || got4[1] != 1 || got4[2] != 0 {
		t.Errorf("2 @ 1/1/1 = %v, want [1 1 0]（平局取下标小者）", got4)
	}
	// Σalloc == N 恒成立
	for n := 0; n <= 30; n++ {
		alloc := LargestRemainder(n, []float64{0.3, 0.3, 0.4})
		sum := alloc[0] + alloc[1] + alloc[2]
		if sum != n {
			t.Fatalf("N=%d 时 Σalloc = %d", n, sum)
		}
	}
}

func TestAllocateProbe(t *testing.T) {
	protos := []config.ProtocolConfig{
		{ID: "openai-chat", Weight: 0.6,
			Transport:    config.TransportMix{Stream: 0.5, NonStream: 0.5},
			Capabilities: config.Capabilities{Stream: true, ContextWindow: 131072}},
		{ID: "anthropic-messages", Weight: 0.4,
			Transport:    config.TransportMix{Stream: 1.0, NonStream: 0.0},
			Capabilities: config.Capabilities{Stream: true, ContextWindow: 200000}},
	}
	alloc := AllocateProbe("onetoken", 0, 20, protos, false, false)
	// openai-chat: 12 → 6 stream + 6 non_stream；anthropic: 8 → 8 stream
	if alloc.CountOf("openai-chat", "stream") != 6 {
		t.Errorf("openai-chat stream = %d, want 6", alloc.CountOf("openai-chat", "stream"))
	}
	if alloc.CountOf("openai-chat", "non_stream") != 6 {
		t.Errorf("openai-chat non_stream = %d, want 6", alloc.CountOf("openai-chat", "non_stream"))
	}
	if alloc.CountOf("anthropic-messages", "stream") != 8 {
		t.Errorf("anthropic stream = %d, want 8", alloc.CountOf("anthropic-messages", "stream"))
	}
}

func TestAllocateProbeContextWindowCuts(t *testing.T) {
	protos := []config.ProtocolConfig{
		{ID: "openai-chat", Weight: 1.0,
			Transport:    config.TransportMix{Stream: 1.0},
			Capabilities: config.Capabilities{Stream: true, ContextWindow: 32000}},
	}
	alloc := AllocateProbe("needle", 128000, 5, protos, false, false)
	if len(alloc.Counts) != 0 {
		t.Errorf("超窗 bucket 不应有分配: %v", alloc.Counts)
	}
	if len(alloc.Skipped) != 1 || alloc.Skipped[0].Reason != "context_window" {
		t.Errorf("skipped = %v", alloc.Skipped)
	}
}

func TestAllocateProbeStreamUnsupported(t *testing.T) {
	protos := []config.ProtocolConfig{
		{ID: "openai-chat", Weight: 1.0,
			Transport:    config.TransportMix{Stream: 0.5, NonStream: 0.5},
			Capabilities: config.Capabilities{Stream: false}},
	}
	alloc := AllocateProbe("onetoken", 0, 10, protos, false, false)
	// stream:false → 流式份额折到非流式
	if alloc.CountOf("openai-chat", "stream") != 0 {
		t.Errorf("stream = %d, want 0", alloc.CountOf("openai-chat", "stream"))
	}
	if alloc.CountOf("openai-chat", "non_stream") != 10 {
		t.Errorf("non_stream = %d, want 10（折算）", alloc.CountOf("openai-chat", "non_stream"))
	}
}

func TestResolveInjection(t *testing.T) {
	cfg := &config.File{}
	cfg.Target.Request.BodyOverrides = map[string]any{
		"vendor_param": "v1",
		"shared":       "from-global",
	}
	proto := config.ProtocolConfig{
		ID: "openai-chat",
		Thinking: config.ThinkingBlock{
			Enabled: config.ThinkingMode{
				EffortMap: map[string]any{"high": 16384},
				BodyOverrides: map[string]any{
					"thinking": map[string]any{"budget_tokens": "${thinking_effort}"},
					"shared":   "from-thinking",
				},
			},
		},
	}

	// 题目无 effort → disabled 块（缺省空）+ 全局 overrides
	tb, err := resolveInjection(cfg, proto, "")
	if err != nil {
		t.Fatal(err)
	}
	if tb.BodyOverrides["vendor_param"] != "v1" {
		t.Errorf("全局 overrides 必须合并进请求体注入: %v", tb.BodyOverrides)
	}
	if _, has := tb.BodyOverrides["thinking"]; has {
		t.Errorf("disabled 不应带 thinking 块: %v", tb.BodyOverrides)
	}

	// 题目 effort=high → 合并顺序：thinking 块覆盖全局；占位符类型保留
	tb2, err := resolveInjection(cfg, proto, "high")
	if err != nil {
		t.Fatal(err)
	}
	if tb2.BodyOverrides["vendor_param"] != "v1" {
		t.Errorf("enabled 注入也应包含全局 overrides: %v", tb2.BodyOverrides)
	}
	if tb2.BodyOverrides["shared"] != "from-thinking" {
		t.Errorf("thinking 块应覆盖全局同名字段: %v", tb2.BodyOverrides["shared"])
	}
	th := tb2.BodyOverrides["thinking"].(map[string]any)
	if th["budget_tokens"] != 16384 {
		t.Errorf("budget_tokens = %v (%T), want 16384 (int) —— 类型必须保留", th["budget_tokens"], th["budget_tokens"])
	}
}

func TestAllocateProbeToolsUnsupported(t *testing.T) {
	protos := []config.ProtocolConfig{
		{ID: "openai-chat", Weight: 0.5,
			Transport:    config.TransportMix{NonStream: 1.0},
			Capabilities: config.Capabilities{Stream: true, Tools: true}},
		{ID: "anthropic-messages", Weight: 0.5,
			Transport:    config.TransportMix{NonStream: 1.0},
			Capabilities: config.Capabilities{Stream: true, Tools: false}},
	}
	// toolcall 需求（needsTools=true）：tools:false 的协议整份额裁掉，进 skipped
	alloc := AllocateProbe("toolcall", 0, 10, protos, true, false)
	if alloc.CountOf("anthropic-messages", "non_stream") != 0 {
		t.Errorf("tools:false 协议不应分到请求: %v", alloc.Counts)
	}
	if alloc.CountOf("openai-chat", "non_stream") != 10 {
		t.Errorf("openai-chat 应拿到全部份额: %v", alloc.Counts)
	}
	found := false
	for _, s := range alloc.Skipped {
		if s.Protocol == "anthropic-messages" && s.Reason == "tools_unsupported" {
			found = true
		}
	}
	if !found {
		t.Errorf("skipped 应含 tools_unsupported: %v", alloc.Skipped)
	}
	// 非 toolcall 探针不受 tools 能力影响
	alloc2 := AllocateProbe("onetoken", 0, 10, protos, false, false)
	if alloc2.CountOf("anthropic-messages", "non_stream") != 5 {
		t.Errorf("onetoken 不应被 tools 裁剪: %v", alloc2.Counts)
	}
}

func TestAllocateProbeThinkingUnsupported(t *testing.T) {
	protos := []config.ProtocolConfig{
		{ID: "openai-chat", Weight: 1.0,
			Transport:    config.TransportMix{NonStream: 1.0},
			Capabilities: config.Capabilities{Stream: true, Thinking: false}},
	}
	// think-effort 需求（needsThinking=true）：thinking:false 的协议整 cell 裁掉
	alloc := AllocateProbe("think-effort", 0, 10, protos, false, true)
	if len(alloc.Counts) != 0 {
		t.Errorf("thinking:false 不应有分配: %v", alloc.Counts)
	}
	if len(alloc.Skipped) != 1 || alloc.Skipped[0].Reason != "thinking_unsupported" {
		t.Errorf("skipped = %v", alloc.Skipped)
	}
}
