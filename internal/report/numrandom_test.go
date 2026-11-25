package report

import (
	"bytes"
	"strings"
	"testing"

	"github.com/chaterm/token-verifier/internal/compare"
	"github.com/chaterm/token-verifier/internal/probe"
)

func TestStdoutAndVerboseNumRandomLabels(t *testing.T) {
	stat := 0.3215
	cellStat := 0.412
	r := stat / 0.15
	res := &compare.Result{
		PlanDigest: "sha256:0123456789abcdef",
		Planned:    1,
		Compared:   1,
		Coverage:   1,
		Verdicts: []probe.Verdict{{
			ProbeID:         "numrandom",
			Verdict:         "fail",
			Statistic:       &stat,
			Threshold:       0.15,
			ThresholdSource: "config",
			Ratio:           &r,
			Note:            "pooled H=0.3215",
			Cells: []probe.CellDetail{{
				Key:  map[string]string{"question_id": "nr.v1.en.001", "context_bucket": "0"},
				NA:   2,
				NB:   2,
				Stat: &cellStat,
				Extra: map[string]any{
					"value_hellinger":      0.43,
					"position_hellinger":   0.22,
					"last_digit_hellinger": 0.11,
					"a_total_ints":         600,
					"b_total_ints":         598,
				},
			}},
		}},
	}

	var out bytes.Buffer
	Stdout(&out, res)
	gotStdout := out.String()
	if !strings.Contains(gotStdout, "0.3215 H") {
		t.Errorf("Stdout 缺少 numrandom 的 H 后缀:\n%s", gotStdout)
	}
	if !strings.Contains(gotStdout, "hellinger 0.412") || strings.Contains(gotStdout, "jsd 0.412") {
		t.Errorf("Stdout 最差 cell 标签应为 hellinger 而非 jsd:\n%s", gotStdout)
	}

	var verb bytes.Buffer
	Verbose(&verb, res)
	gotVerbose := verb.String()
	if !strings.Contains(gotVerbose, "hellinger: val=0.43 pos=0.22 dig=0.11  ints: A=600 B=598") {
		t.Errorf("Verbose 缺少 numrandom 紧凑摘要:\n%s", gotVerbose)
	}
}
