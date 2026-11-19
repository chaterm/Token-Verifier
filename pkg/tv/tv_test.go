package tv_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/chaterm/token-verifier/pkg/tv"
)

const sampleSuiteYAML = `suite_version: v1
output_contract:
  field: answer
  system_prompt: '请只回答 JSON {"answer": "<your answer>"}'
probes:
  onetoken:
    normalize: number
    items:
      - id: sdk.ot.001
        prompt: "Pick 1..10"
  tokenizer:
    normalize: none
    items:
      - id: sdk.tk.001
        prompt: "Hello 世界 🚀"
`

func TestEmbeddedSDKLifecycleWithoutEnvOrDiskSuite(t *testing.T) {
	st, err := tv.ParseSuite([]byte(sampleSuiteYAML), "")
	if err != nil {
		t.Fatalf("ParseSuite: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-inline-memory-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": `{"answer":"7"}`}, "finish_reason": "stop"},
			},
			"usage": map[string]any{
				"prompt_tokens":     24,
				"completion_tokens": 5,
			},
		})
	}))
	defer srv.Close()

	tempOne := 1.0
	tempZero := 0.0
	maxTok := 16
	cfg := &tv.ConfigFile{
		Version: 1,
		Target: tv.TargetConfig{
			BaseURL: srv.URL,
			Model:   "sdk-test-model",
			Protocols: []tv.ProtocolConfig{{
				ID:        "openai-chat",
				Weight:    1,
				Transport: tv.TransportMix{NonStream: 1.0},
				Thinking:  tv.ThinkingBlock{Disabled: tv.ThinkingMode{BodyOverrides: map[string]any{}}},
			}},
		},
		Probes: map[string]tv.ProbeConfig{
			"onetoken":  {Enabled: true, Repeats: 10, Temperature: &tempOne, MaxTokens: &maxTok, ContextBuckets: []int{0}},
			"tokenizer": {Enabled: true, Repeats: 1, Temperature: &tempZero, MaxTokens: &maxTok, ContextBuckets: []int{0}},
		},
	}

	if _, err := tv.ValidateInMemory(cfg, st, true); err != nil {
		t.Fatalf("ValidateInMemory: %v", err)
	}

	summary, err := tv.BuildPlanSummary(cfg, st)
	if err != nil {
		t.Fatalf("BuildPlanSummary: %v", err)
	}
	if summary.PlanDigest == "" || summary.TotalRequests != 11 {
		t.Fatalf("unexpected summary: %+v", summary)
	}

	probes, err := tv.ProbeEndpoint(context.Background(), cfg.Target, "sk-inline-memory-key", 5)
	if err != nil || len(probes) != 1 || !probes[0].Success {
		t.Fatalf("ProbeEndpoint failed: %+v, err=%v", probes, err)
	}

	outA := filepath.Join(t.TempDir(), "a.rawdata.jsonl.gz")
	resA, err := tv.Collect(context.Background(), cfg, st, tv.CollectOptions{
		OutputPath: outA,
		APIKey:     "sk-inline-memory-key",
	})
	if err != nil || resA.SuccessCount != 11 {
		t.Fatalf("Collect A: res=%+v err=%v", resA, err)
	}

	outB := filepath.Join(t.TempDir(), "b.rawdata.jsonl.gz")
	if _, err := tv.Collect(context.Background(), cfg, st, tv.CollectOptions{
		OutputPath: outB,
		APIKey:     "sk-inline-memory-key",
	}); err != nil {
		t.Fatalf("Collect B: %v", err)
	}

	fA, err := os.Open(outA)
	if err != nil {
		t.Fatal(err)
	}
	defer fA.Close()
	rdA, err := tv.ReadRawDataStream(fA)
	if err != nil {
		t.Fatalf("ReadRawDataStream: %v", err)
	}
	rdB, err := tv.ReadRawData(outB)
	if err != nil {
		t.Fatalf("ReadRawData: %v", err)
	}

	cmp, err := tv.Compare(rdA, rdB, map[string]float64{
		"onetoken":  0.15,
		"tokenizer": 0.01,
	}, nil, tv.CompareOptions{})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if cmp.Compared != 2 {
		t.Fatalf("expected 2 compared probes, got %d", cmp.Compared)
	}
}
