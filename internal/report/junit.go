package report

import (
	"encoding/xml"
	"fmt"
	"io"
	"strings"

	"github.com/chaterm/token-verifier/internal/compare"
)

// JUnit 输出 JUnit XML 报告：整个比较是一个 testsuite。
// 单档位探针一个 testcase（name=探针 ID）；多档位探针每档位一个
// testcase（name="探针[context_bucket=N]"），便于 CI 直接定位回归档位。
// fail 带 <failure>，inconclusive 带 <skipped>。
func JUnit(w io.Writer, res *compare.Result) error {
	suite := junitSuite{
		Name: "token-verifier compare",
	}
	for _, v := range res.Verdicts {
		if len(v.Buckets) > 1 {
			for _, bv := range v.Buckets {
				name := fmt.Sprintf("%s[context_bucket=%d]", v.ProbeID, bv.ContextBucket)
				tc := junitTestCase{Name: name, Classname: "token-verifier"}
				switch bv.Verdict {
				case "fail":
					suite.Failures++
					msg := fmt.Sprintf("statistic=%s threshold=%s",
						statisticText(v.ProbeID, bv.Statistic), thresholdText(v.ProbeID, bv.Threshold))
					if bv.Warning != "" {
						msg += "; warning: " + bv.Warning
					}
					tc.Failure = &junitFailure{Message: msg, Type: "probe-failed"}
				case "inconclusive":
					suite.Skipped++
					tc.Skipped = &junitSkipped{Message: bv.Note}
				}
				suite.Tests++
				suite.Cases = append(suite.Cases, tc)
			}
			continue
		}
		tc := junitTestCase{
			Name:      v.ProbeID,
			Classname: "token-verifier",
		}
		switch v.Verdict {
		case "fail":
			suite.Failures++
			msg := fmt.Sprintf("statistic=%s threshold=%s",
				statisticText(v.ProbeID, v.Statistic), thresholdText(v.ProbeID, v.Threshold))
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
		suite.Tests++
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
	// 子集模式：比较范围进 properties，CI 消费 XML 时同样能看到交集收缩
	if sc := res.Scope; sc != nil {
		suite.Properties = append(suite.Properties, junitProperty{
			Name: "subset", Value: "true",
		}, junitProperty{
			Name: "plan_digest_b", Value: sc.DigestB,
		}, junitProperty{
			Name: "scope.probes", Value: strings.Join(sc.Probes, ","),
		})
		for _, ex := range sc.Excluded {
			name := "scope.excluded." + ex.ProbeID
			if ex.Kind == "bucket" && ex.Bucket != nil {
				name = fmt.Sprintf("scope.excluded.%s.bucket_%d", ex.ProbeID, *ex.Bucket)
			}
			suite.Properties = append(suite.Properties, junitProperty{Name: name, Value: ex.Reason})
		}
	}

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
