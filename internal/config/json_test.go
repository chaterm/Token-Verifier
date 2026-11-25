package config_test

import (
	"encoding/json"
	"testing"

	"github.com/chaterm/token-verifier/internal/config"
)

// capabilities 与 YAML 一样，必须区分「未填写（默认全支持）」与「显式全部关闭」。
func TestCapabilitiesJSONDistinguishesMissingFromExplicitFalse(t *testing.T) {
	var defaults config.Capabilities
	if err := json.Unmarshal([]byte(`{}`), &defaults); err != nil {
		t.Fatal(err)
	}
	if !defaults.Present || !defaults.Stream || !defaults.Tools || !defaults.Thinking || !defaults.Temperature {
		t.Errorf("空对象应应用缺省能力并标记 Present: %+v", defaults)
	}

	var disabled config.Capabilities
	if err := json.Unmarshal([]byte(`{"stream":false,"tools":false,"thinking":false,"temperature":false,"context_window":0}`), &disabled); err != nil {
		t.Fatal(err)
	}
	if !disabled.Present || disabled.Stream || disabled.Tools || disabled.Thinking || disabled.Temperature {
		t.Errorf("显式全 false 不应被翻转成缺省 true: %+v", disabled)
	}
}

// TV-Watch 通过 JSON 使用 SDK 配置类型，snake_case 字段必须与 YAML 一样完整往返。
func TestConfigStructsSnakeCaseJSONRoundTrip(t *testing.T) {
	var probe config.ProbeConfig
	probeJSON := `{"enabled":true,"repeats":10,"temperature":1.0,"max_tokens":16,"context_buckets":[0,8000],"min_n":10,"thinking_effort":"low"}`
	if err := json.Unmarshal([]byte(probeJSON), &probe); err != nil {
		t.Fatal(err)
	}
	if len(probe.ContextBuckets) != 2 || probe.ContextBuckets[0] != 0 || probe.ContextBuckets[1] != 8000 {
		t.Errorf("context_buckets 丢失: %+v", probe.ContextBuckets)
	}
	if probe.MaxTokens == nil || *probe.MaxTokens != 16 {
		t.Errorf("max_tokens 丢失: %+v", probe.MaxTokens)
	}
	if probe.MinN == nil || *probe.MinN != 10 {
		t.Errorf("min_n 丢失: %+v", probe.MinN)
	}
	if probe.ThinkingEffort == nil || *probe.ThinkingEffort != "low" {
		t.Errorf("thinking_effort 丢失: %+v", probe.ThinkingEffort)
	}

	var protocol config.ProtocolConfig
	protocolJSON := `{"id":"openai-chat","weight":1,"transport":{"stream":0.25,"non_stream":0.75},"thinking":{"enabled":{"effort_map":{"low":4096},"body_overrides":{"thinking":{"budget_tokens":"${thinking_effort}"}}}}}`
	if err := json.Unmarshal([]byte(protocolJSON), &protocol); err != nil {
		t.Fatal(err)
	}
	if protocol.Transport.NonStream != 0.75 {
		t.Errorf("non_stream 丢失: %+v", protocol.Transport)
	}
	if protocol.Thinking.Enabled.EffortMap["low"] != float64(4096) {
		t.Errorf("effort_map 丢失: %+v", protocol.Thinking.Enabled.EffortMap)
	}
	if protocol.Thinking.Enabled.BodyOverrides["thinking"] == nil {
		t.Errorf("body_overrides 丢失: %+v", protocol.Thinking.Enabled.BodyOverrides)
	}

	var request config.RequestConfig
	requestJSON := `{"temperature_field":"generation_config.temperature","body_overrides":{"provider":"demo"}}`
	if err := json.Unmarshal([]byte(requestJSON), &request); err != nil {
		t.Fatal(err)
	}
	if request.TemperatureField != "generation_config.temperature" || request.BodyOverrides["provider"] != "demo" {
		t.Errorf("request 字段丢失: %+v", request)
	}

	for name, value := range map[string]any{
		"probe":    probe,
		"protocol": protocol,
		"request":  request,
	} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var keys map[string]any
		if err := json.Unmarshal(raw, &keys); err != nil {
			t.Fatal(err)
		}
		switch name {
		case "probe":
			for _, key := range []string{"max_tokens", "context_buckets", "min_n", "thinking_effort"} {
				if _, ok := keys[key]; !ok {
					t.Errorf("%s 序列化缺少 %s: %s", name, key, raw)
				}
			}
		case "protocol":
			var transport map[string]any
			rawTransport, _ := json.Marshal(protocol.Transport)
			_ = json.Unmarshal(rawTransport, &transport)
			if _, ok := transport["non_stream"]; !ok {
				t.Errorf("transport 序列化缺少 non_stream: %s", rawTransport)
			}
		case "request":
			for _, key := range []string{"temperature_field", "body_overrides"} {
				if _, ok := keys[key]; !ok {
					t.Errorf("%s 序列化缺少 %s: %s", name, key, raw)
				}
			}
		}
	}
}
