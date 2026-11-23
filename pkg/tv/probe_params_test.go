package tv_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/chaterm/token-verifier/pkg/tv"
)

func TestProbeEndpointOmitsTemperatureForFixedTempModels(t *testing.T) {
	var sawTempKey bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if _, ok := body["temperature"]; ok {
			sawTempKey = true
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid temperature: only 0.6 is allowed for this model"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": "Hello TV-Watch"}, "finish_reason": "stop"},
			},
			"usage": map[string]any{"prompt_tokens": 12, "completion_tokens": 4},
		})
	}))
	defer srv.Close()

	target := tv.TargetConfig{
		BaseURL: srv.URL,
		Model:   "kimi-k3.1-pro",
		Protocols: []tv.ProtocolConfig{{
			ID:           "openai-chat",
			Weight:       1,
			Capabilities: tv.Capabilities{Stream: true, Tools: true, Thinking: true, Present: true},
			Transport:    tv.TransportMix{NonStream: 1.0},
		}},
	}
	items, err := tv.ProbeEndpoint(context.Background(), target, "sk-kimi-test", 5)
	if err != nil {
		t.Fatalf("ProbeEndpoint error: %v", err)
	}
	if sawTempKey {
		t.Fatalf("ProbeEndpoint should omit temperature by default so fixed-temp models pass connectivity check")
	}
	if len(items) != 1 || !items[0].Success {
		t.Fatalf("expected ProbeEndpoint success, got %+v", items)
	}
}

func TestProbeEndpointParamsKimiLikeGateway(t *testing.T) {
	// 模拟类似 Kimi K3.1 Pro 的网关行为：
	// - temperature 只允许省略或 0.6；传 0 或 1 报 400
	// - reasoning_effort 只接受 low/high/max（不接受 none / medium / xhigh）
	// - thinking.type=disabled 与 enable_thinking=false 返回 200 但静默忽略（始终产生 reasoning_tokens）
	var mu sync.Mutex
	seenBodies := []map[string]any{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		seenBodies = append(seenBodies, body)
		mu.Unlock()

		if rawTemp, hasTemp := body["temperature"]; hasTemp {
			if f, ok := rawTemp.(float64); !ok || f != 0.6 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"message":"invalid temperature: only 0.6 is allowed for this model"}}`))
				return
			}
		}

		reasoningTokens := 90
		if eff, ok := body["reasoning_effort"].(string); ok {
			switch eff {
			case "low":
				reasoningTokens = 40
			case "high":
				reasoningTokens = 160
			case "max":
				reasoningTokens = 320
			default:
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"message":"unsupported reasoning_effort: ` + eff + `"}}`))
				return
			}
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{
					"message": map[string]any{
						"content":           "391",
						"reasoning_content": "17*23=391",
					},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]any{
				"prompt_tokens":     20,
				"completion_tokens": 10 + reasoningTokens,
				"completion_tokens_details": map[string]any{
					"reasoning_tokens": reasoningTokens,
				},
			},
		})
	}))
	defer srv.Close()

	target := tv.TargetConfig{
		BaseURL: srv.URL,
		Model:   "kimi-k3.1-pro",
		Request: tv.RequestConfig{
			TemperatureField: "temperature",
			// 刻意放一个旧 temperature override，确认 ProbeEndpointParams 会隔离剥离它
			BodyOverrides: map[string]any{"temperature": 0.6, "user": "probe"},
		},
		Protocols: []tv.ProtocolConfig{{
			ID:           "openai-chat",
			Weight:       1,
			Capabilities: tv.Capabilities{Stream: false, Tools: true, Thinking: true, Present: true},
			Transport:    tv.TransportMix{NonStream: 1.0},
			Thinking: tv.ThinkingBlock{
				Disabled: tv.ThinkingMode{BodyOverrides: map[string]any{"reasoning_effort": "none"}},
				Enabled: tv.ThinkingMode{
					EffortMap:     map[string]any{"low": "low", "medium": "high", "high": "max"},
					BodyOverrides: map[string]any{"reasoning_effort": "${thinking_effort}"},
				},
			},
		}},
	}

	res, err := tv.ProbeEndpointParams(context.Background(), target, "sk-kimi-secret", tv.ParamProbeOptions{
		ExtraTemperatures: []float64{0.6},
	})
	if err != nil {
		t.Fatalf("ProbeEndpointParams failed: %v", err)
	}

	// 不得原地污染 target.Request.BodyOverrides
	if target.Request.BodyOverrides["temperature"] != 0.6 {
		t.Fatalf("caller target.Request.BodyOverrides mutated: %+v", target.Request.BodyOverrides)
	}

	if res.TemperatureSummary.OmittedOK == nil || !*res.TemperatureSummary.OmittedOK {
		t.Errorf("expected omitted temperature OK, got %+v", res.TemperatureSummary)
	}
	if len(res.TemperatureSummary.Accepted) != 1 || res.TemperatureSummary.Accepted[0] != 0.6 {
		t.Errorf("expected only 0.6 accepted, got %+v", res.TemperatureSummary.Accepted)
	}
	if len(res.TemperatureSummary.Rejected) != 2 {
		t.Errorf("expected 0 and 1 rejected, got %+v", res.TemperatureSummary.Rejected)
	}

	// 默认即产出 reasoning，且没有任何关闭方式真正把 reasoning 降为 0（always-on）
	if res.ThinkingSummary.DefaultProducesReasoning == nil || !*res.ThinkingSummary.DefaultProducesReasoning {
		t.Errorf("expected default_produces_reasoning=true, got %+v", res.ThinkingSummary)
	}
	if len(res.ThinkingSummary.DisableEffective) != 0 {
		t.Errorf("expected no effective disable method for always-on model, got %+v", res.ThinkingSummary.DisableEffective)
	}
	if len(res.ThinkingSummary.DisableAccepted) != 2 {
		t.Errorf("expected thinking.type=disabled & enable_thinking=false accepted (200), got %+v", res.ThinkingSummary.DisableAccepted)
	}
	if res.ThinkingSummary.ConfiguredDisabledOK == nil || *res.ThinkingSummary.ConfiguredDisabledOK {
		t.Errorf("expected configured:disabled (reasoning_effort=none) rejected, got %+v", res.ThinkingSummary.ConfiguredDisabledOK)
	}

	// effort 接受 low/high/max，拒绝 medium/xhigh
	if len(res.ThinkingSummary.EffortAccepted) != 3 {
		t.Errorf("expected 3 accepted efforts (low/high/max), got %+v", res.ThinkingSummary.EffortAccepted)
	}
	if len(res.ThinkingSummary.EffortRejected) != 2 {
		t.Errorf("expected 2 rejected efforts (medium/xhigh), got %+v", res.ThinkingSummary.EffortRejected)
	}
	for _, eff := range res.ThinkingSummary.EffortAccepted {
		if eff.Value == "max" && eff.ReasoningTokens != 320 {
			t.Errorf("expected max reasoning_tokens=320, got %d", eff.ReasoningTokens)
		}
	}

	// 校验自定义保留字段拦截
	_, err = tv.ProbeEndpointParams(context.Background(), target, "sk-kimi-secret", tv.ParamProbeOptions{
		CustomCases: []tv.ParamProbeCustomCase{
			{Label: "bad", BodyOverrides: map[string]any{"model": "hack"}},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "保留字段") {
		t.Errorf("expected reserved field rejection, got %v", err)
	}
}

func TestProbeEndpointParamsEffectiveDisableAndAutoTempFallback(t *testing.T) {
	// 模拟要求必须显式传 temperature=0.6 且支持 enable_thinking=false 真正关闭思考的网关
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)

		rawTemp, hasTemp := body["temperature"]
		if !hasTemp || rawTemp != 0.6 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"temperature 0.6 is required"}}`))
			return
		}
		reasoningTokens := 120
		if et, ok := body["enable_thinking"].(bool); ok && !et {
			reasoningTokens = 0
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": "391"}, "finish_reason": "stop"},
			},
			"usage": map[string]any{
				"prompt_tokens":     15,
				"completion_tokens": 5 + reasoningTokens,
				"completion_tokens_details": map[string]any{
					"reasoning_tokens": reasoningTokens,
				},
			},
		})
	}))
	defer srv.Close()

	target := tv.TargetConfig{
		BaseURL: srv.URL,
		Model:   "strict-qwen",
		Protocols: []tv.ProtocolConfig{{
			ID:           "openai-chat",
			Weight:       1,
			Capabilities: tv.Capabilities{Stream: false, Tools: true, Thinking: true, Present: true},
			Transport:    tv.TransportMix{NonStream: 1.0},
		}},
	}

	emptyEfforts := []string{}
	includeCfg := false
	res, err := tv.ProbeEndpointParams(context.Background(), target, "sk-test", tv.ParamProbeOptions{
		ExtraTemperatures: []float64{0.6},
		IncludeConfigured: &includeCfg,
		EffortCandidates:  &emptyEfforts,
	})
	if err != nil {
		t.Fatalf("ProbeEndpointParams failed: %v", err)
	}
	if res.TemperatureSummary.OmittedOK == nil || *res.TemperatureSummary.OmittedOK {
		t.Fatalf("expected omitted temp rejected")
	}
	if res.ThinkingSummary.EffectiveTemperature == nil || *res.ThinkingSummary.EffectiveTemperature != 0.6 {
		t.Fatalf("expected thinking stage to auto-fallback to 0.6, got %+v", res.ThinkingSummary.EffectiveTemperature)
	}
	if len(res.ThinkingSummary.DisableEffective) != 1 || res.ThinkingSummary.DisableEffective[0] != "enable_thinking=false" {
		t.Fatalf("expected enable_thinking=false effective, got %+v", res.ThinkingSummary.DisableEffective)
	}
}
