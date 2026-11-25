package tv

import (
	"path/filepath"
	"testing"
)

func TestNumRandomSDKAndExampleFiles(t *testing.T) {
	found := false
	for _, id := range AllProbeIDs() {
		if id == "numrandom" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("AllProbeIDs() 缺少 numrandom: %v", AllProbeIDs())
	}

	pr, ok := GetProbe("numrandom")
	if !ok {
		t.Fatal("GetProbe(\"numrandom\") 返回 false")
	}
	if pr.Meta().StatKind != StatDistance || pr.Meta().MinN != 1 {
		t.Fatalf("unexpected numrandom meta: %+v", pr.Meta())
	}

	// 验证仓库示例题库 suites/v1.example.yaml 可被完整加载且包含 numrandom
	suitePath := filepath.Join("..", "..", "suites", "v1.example.yaml")
	st, err := LoadSuite(suitePath, "")
	if err != nil {
		t.Fatalf("LoadSuite(%s): %v", suitePath, err)
	}
	ps, ok := st.ProbeSetOf("numrandom")
	if !ok || len(ps.Items) < 2 {
		t.Fatalf("v1.example.yaml 应含 numrandom 示例题目: ok=%v len=%d", ok, len(ps.Items))
	}

	// 验证启用 numrandom 时 BuildPlanSummary 产出正确请求数与 digest
	cfg := &ConfigFile{
		Version: 1,
		Probes: map[string]ProbeConfig{
			"numrandom": {
				Enabled:        true,
				Repeats:        2,
				ContextBuckets: []int{0},
			},
		},
	}
	sum, err := BuildPlanSummary(cfg, st)
	if err != nil {
		t.Fatalf("BuildPlanSummary: %v", err)
	}
	if sum.ProbeRequests["numrandom"] != len(ps.Items)*2 {
		t.Errorf("ProbeRequests[numrandom] = %d, want %d", sum.ProbeRequests["numrandom"], len(ps.Items)*2)
	}
	if sum.ProbeDigests["numrandom"] == "" {
		t.Errorf("ProbeDigests[numrandom] 不应为空")
	}
}
