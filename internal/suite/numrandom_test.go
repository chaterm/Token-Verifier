package suite

import (
	"strings"
	"testing"
)

func TestNumRandomSuiteSpecAndValidation(t *testing.T) {
	validYAML := `suite_version: v1
output_contract:
  field: answer
  system_prompt: 'JSON only'
probes:
  numrandom:
    normalize: none
    thinking_effort: null
    min_value: 1
    max_value: 355
    items:
      - id: nr.v1.001
        target_count: 292
        prompt: "Pick 292 integers in 1..355"
      - id: nr.v1.002
        min_value: 10
        max_value: 200
        target_count: 120
        prompt: "Pick 120 integers in 10..200"
`
	f, err := Parse([]byte(validYAML), "")
	if err != nil {
		t.Fatalf("合法 numrandom 题库不应报错: %v", err)
	}
	ps, ok := f.ProbeSetOf("numrandom")
	if !ok {
		t.Fatal("缺少 numrandom ProbeSet")
	}
	s1 := ps.NumRandomSpecOf(ps.Items[0])
	if s1.MinValue != 1 || s1.MaxValue != 355 || s1.TargetCount != 292 {
		t.Errorf("继承探针级值域错误: %+v", s1)
	}
	s2 := ps.NumRandomSpecOf(ps.Items[1])
	if s2.MinValue != 10 || s2.MaxValue != 200 || s2.TargetCount != 120 {
		t.Errorf("题目级覆盖值域错误: %+v", s2)
	}

	// 未写 min_value/max_value 时缺省 1..355
	defaultRangeYAML := `suite_version: v1
probes:
  numrandom:
    normalize: none
    items:
      - id: nr.v1.def
        target_count: 300
        prompt: "Pick 300 integers"
`
	fd, err := Parse([]byte(defaultRangeYAML), "")
	if err != nil {
		t.Fatalf("缺省值域应合法: %v", err)
	}
	psd, _ := fd.ProbeSetOf("numrandom")
	sd := psd.NumRandomSpecOf(psd.Items[0])
	if sd.MinValue != 1 || sd.MaxValue != 355 || sd.TargetCount != 300 {
		t.Errorf("缺省值域 = %+v, want 1..355 count 300", sd)
	}

	errCases := []struct {
		name    string
		yaml    string
		wantSub string
	}{
		{
			name: "non_none_normalize_rejected",
			yaml: `suite_version: v1
output_contract:
  field: answer
  system_prompt: 'JSON only'
probes:
  numrandom:
    normalize: number
    items:
      - id: nr.bad.1
        target_count: 300
        prompt: "p"
`,
			wantSub: "normalize",
		},
		{
			name: "missing_target_count",
			yaml: `suite_version: v1
probes:
  numrandom:
    normalize: none
    items:
      - id: nr.bad.2
        prompt: "p"
`,
			wantSub: "target_count",
		},
		{
			name: "range_too_narrow",
			yaml: `suite_version: v1
probes:
  numrandom:
    normalize: none
    min_value: 1
    max_value: 10
    items:
      - id: nr.bad.3
        target_count: 100
        prompt: "p"
`,
			wantSub: "max_value",
		},
	}
	for _, tc := range errCases {
		if _, err := Parse([]byte(tc.yaml), ""); err == nil || !strings.Contains(err.Error(), tc.wantSub) {
			t.Errorf("%s: err = %v, want substring %q", tc.name, err, tc.wantSub)
		}
	}
}
