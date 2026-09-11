package report

import (
	"encoding/xml"
	"fmt"
	"io"

	"github.com/chaterm/token-verifier/internal/compare"
)

// JUnit 输出 JUnit XML 报告：整个比较是一个 testsuite，每个探针一个 testcase，
// fail 带 <failure>，inconclusive 带 <skipped>，便于 CI 直接消费。
func JUnit(w io.Writer, res *compare.Result) error {
	suite := junitSuite{
		Name:  "token-verifier compare",
		Tests: len(res.Verdicts),
	}
	for _, v := range res.Verdicts {
		tc := junitTestCase{
			Name:      v.ProbeID,
			Classname: "token-verifier",
		}
		switch v.Verdict {
		case "fail":
			suite.Failures++
			msg := fmt.Sprintf("statistic=%s threshold=%s", statisticText(v), thresholdText(v))
			if worst := worstCell(v); worst != nil {
				msg += "; " + *worst
			}
			if v.Warning != "" {
				msg += "; warning: " + v.Warning
			}
			tc.Failure = &junitFailure{Message: msg, Type: "probe-failed"}
		case "inconclusive":
			suite.Skipped++
			tc.Skipped = &junitSkipped{Message: v.Note}
		}
		// stdout 报告字段：pass 的 testcase 无子元素
		suite.Cases = append(suite.Cases, tc)
	}
	// 附注输出到 properties，保证 sampling 差异等信息在 XML 里也可见
	for i, n := range res.Notes {
		suite.Properties = append(suite.Properties, junitProperty{
			Name:  fmt.Sprintf("note.%d", i),
			Value: n,
		})
	}
	suite.Properties = append(suite.Properties, junitProperty{
		Name: "plan_digest", Value: res.PlanDigest,
	}, junitProperty{
		Name: "coverage", Value: fmt.Sprintf("%d/%d", res.Compared, res.Planned),
	})

	out, err := xml.MarshalIndent(suite, "", "  ")
	if err != nil {
		return fmt.Errorf("JUnit 报告序列化失败: %w", err)
	}
	if _, err := w.Write([]byte(xml.Header)); err != nil {
		return err
	}
	_, err = w.Write(append(out, '\n'))
	return err
}

type junitSuite struct {
	XMLName    xml.Name        `xml:"testsuite"`
	Name       string          `xml:"name,attr"`
	Tests      int             `xml:"tests,attr"`
	Failures   int             `xml:"failures,attr"`
	Skipped    int             `xml:"skipped,attr"`
	Properties []junitProperty `xml:"properties>property,omitempty"`
	Cases      []junitTestCase `xml:"testcase"`
}

type junitProperty struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

type junitTestCase struct {
	Name      string        `xml:"name,attr"`
	Classname string        `xml:"classname,attr"`
	Failure   *junitFailure `xml:"failure,omitempty"`
	Skipped   *junitSkipped `xml:"skipped,omitempty"`
}

type junitFailure struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
}

type junitSkipped struct {
	Message string `xml:"message,attr"`
}
