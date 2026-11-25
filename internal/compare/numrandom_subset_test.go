package compare

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestReconcileNumRandomStrictItems(t *testing.T) {
	mkRaw := func(target int) json.RawMessage {
		m := map[string]any{
			"suite_version":        "v1",
			"question_set_digest":  "sha256:abc",
			"padding_algo_version": "v1",
			"padding_seed":         12345,
			"probes": map[string]any{
				"numrandom": map[string]any{
					"probe_version":      "1",
					"repeats":            2,
					"temperature":        nil,
					"top_p":              nil,
					"max_tokens":         nil,
					"context_buckets":    []int{0},
					"thinking_effort":    nil,
					"normalize_rule":     "none",
					"observation_schema": "numrandom/v1",
					"cell_key":           []string{"question_id", "context_bucket"},
					"question_ids":       []string{"nr.1"},
					"min_n":              1,
					"numrandom_items": map[string]any{
						"nr.1": map[string]any{"min_value": 1, "max_value": 355, "target_count": target},
					},
				},
			},
		}
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	_, conflicts, err := ReconcilePlans(mkRaw(292), mkRaw(312), "d1", "d2")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range conflicts {
		if strings.HasSuffix(c.Path, ".numrandom_items") {
			found = true
		}
	}
	if !found {
		t.Fatalf("numrandom_items 不一致应在子集模式下产生 strict 冲突: %+v", conflicts)
	}
}
