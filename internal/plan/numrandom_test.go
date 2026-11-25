package plan

import (
	"testing"

	"github.com/chaterm/token-verifier/internal/config"
	"github.com/chaterm/token-verifier/internal/suite"
)

func TestNumRandomPlanDigestIsolationAndSensitivity(t *testing.T) {
	// 1. 既有五探针计划的 canonical JSON 与 digest 完全不受新增 omitempty 字段影响
	stLegacy := minSuite()
	cfgLegacy := minimalCfg(t, stLegacy)
	cpLegacy, err := Build(cfgLegacy, stLegacy)
	if err != nil {
		t.Fatal(err)
	}
	_, canonLegacy, err := cpLegacy.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if string(canonLegacy) == "" {
		t.Fatal("empty canonical JSON")
	}
	for pid, pp := range cpLegacy.Probes {
		if len(pp.NumRandomItems) != 0 {
			t.Errorf("非 numrandom 探针 %s 不应携带 NumRandomItems: %+v", pid, pp.NumRandomItems)
		}
	}

	// 2. numrandom 的 target_count / min_value / max_value 进 digest，修改任一值必须改变 plan_digest
	makeNRPlan := func(target int) string {
		st := &suite.File{
			SuiteVersion: "v1",
			Probes: map[string]suite.ProbeSet{
				"numrandom": {
					Normalize: "none",
					MinValue:  1,
					MaxValue:  355,
					Items: []suite.Item{
						{ID: "nr.1", Prompt: "p", TargetCount: target},
					},
				},
			},
		}
		cfg := minimalCfg(t, st)
		cfg.Probes = map[string]config.ProbeConfig{
			"numrandom": {Enabled: true, Repeats: 2, ContextBuckets: []int{0}},
		}
		cp, err := Build(cfg, st)
		if err != nil {
			t.Fatal(err)
		}
		d, _, err := cp.Digest()
		if err != nil {
			t.Fatal(err)
		}
		return d
	}

	d292 := makeNRPlan(292)
	d312 := makeNRPlan(312)
	if d292 == d312 {
		t.Fatalf("target_count 改变必须改变 plan_digest: %s == %s", d292, d312)
	}
}
