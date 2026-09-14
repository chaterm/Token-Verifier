package report

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"

	"github.com/chaterm/token-verifier/internal/compare"
	"github.com/chaterm/token-verifier/internal/stats"
)

// jsonReport JSON 报告的顶层结构，对齐 README 的报告示例。
type jsonReport struct {
	PlanDigest string        `json:"plan_digest"`
	Coverage   float64       `json:"coverage"`
	Compared   int           `json:"compared"`
	Planned    int           `json:"planned"`
	Probes     []jsonProbe   `json:"probes"`
	Transport  jsonTransport `json:"transport"`
	// TransportHistograms 传输指标的两侧联合直方图（描述性证据）：
	// A/B 共享 edges，counts 逐 bin 对齐；无样本的指标整个为 null。
	TransportHistograms jsonTransportHists `json:"transport_histograms"`
	Notes               []string           `json:"notes,omitempty"`
}

// jsonTransportHists 三个传输指标的直方图。
type jsonTransportHists struct {
	LatencyMs *stats.Hist `json:"latency_ms"`
	TtftMs    *stats.Hist `json:"ttft_ms"`
	Tps       *stats.Hist `json:"tps"`
}

type jsonProbe struct {
	ProbeID         string       `json:"probe_id"`
	Verdict         string       `json:"verdict"`
	Statistic       *float64     `json:"statistic"`
	Threshold       float64      `json:"threshold"`
	ThresholdSource string       `json:"threshold_source,omitempty"`
	Ratio           *float64     `json:"ratio"`
	Note            string       `json:"note,omitempty"`
	Warning         string       `json:"warning,omitempty"`
	Buckets         []jsonBucket `json:"buckets,omitempty"`
	Cells           []jsonCell   `json:"cells,omitempty"`
}

// jsonBucket 单个上下文档位的子判定（多档位比较时输出；单档位不输出）。
type jsonBucket struct {
	ContextBucket   int      `json:"context_bucket"`
	Verdict         string   `json:"verdict"`
	Statistic       *float64 `json:"statistic"`
	Threshold       float64  `json:"threshold"`
	ThresholdSource string   `json:"threshold_source,omitempty"`
	Ratio           *float64 `json:"ratio"`
	Note            string   `json:"note,omitempty"`
	Warning         string   `json:"warning,omitempty"`
}

// jsonCell 单 cell 明细：cell_key 各分量平铺在顶层，两侧样本量放 a/b。
type jsonCell struct {
	Key          map[string]string
	NA           int
	NB           int
	Stat         *float64
	Insufficient bool
	Extra        map[string]any
}

// MarshalJSON 把 cell 渲染为平铺对象：键分量 + a/b 样本量 + 统计量 + 探针特有字段。
func (c jsonCell) MarshalJSON() ([]byte, error) {
	out := make(map[string]any, len(c.Key)+5)
	for k, v := range c.Key {
		// context_bucket 是数字，还原为 int，与 README 报告示例一致
		if k == "context_bucket" {
			if n, err := strconv.Atoi(v); err == nil {
				out[k] = n
				continue
			}
		}
		out[k] = v
	}
	out["a"] = map[string]any{"n": c.NA}
	out["b"] = map[string]any{"n": c.NB}
	if c.Stat != nil {
		out["stat"] = *c.Stat
	}
	if c.Insufficient {
		out["insufficient"] = true
	}
	for k, v := range c.Extra {
		out[k] = v
	}
	return json.Marshal(out)
}

type jsonTransport struct {
	A jsonTransportSide `json:"a"`
	B jsonTransportSide `json:"b"`
}

type jsonTransportSide struct {
	Availability float64         `json:"availability"`
	ErrorRate    float64         `json:"error_rate"`
	TimeoutRate  float64         `json:"timeout_rate"`
	LatencyMs    jsonPercentiles `json:"latency_ms"`
	TtftMs       jsonPercentiles `json:"ttft_ms"`
	Tps          jsonPercentiles `json:"tps"`
}

type jsonPercentiles struct {
	P50 *float64 `json:"p50"`
	P90 *float64 `json:"p90"`
	P99 *float64 `json:"p99"`
}

// JSON 把比较结果写成缩进 JSON。
func JSON(w io.Writer, res *compare.Result) error {
	rep := jsonReport{
		PlanDigest: res.PlanDigest,
		Coverage:   res.Coverage,
		Compared:   res.Compared,
		Planned:    res.Planned,
		Notes:      res.Notes,
		Transport: jsonTransport{
			A: toSide(res.TransportA),
			B: toSide(res.TransportB),
		},
		TransportHistograms: jsonTransportHists{
			LatencyMs: histOrNil(res.Hists.LatencyMs),
			TtftMs:    histOrNil(res.Hists.TtftMs),
			Tps:       histOrNil(res.Hists.Tps),
		},
	}
	for _, v := range res.Verdicts {
		jp := jsonProbe{
			ProbeID:         v.ProbeID,
			Verdict:         v.Verdict,
			Statistic:       v.Statistic,
			Threshold:       v.Threshold,
			ThresholdSource: v.ThresholdSource,
			Ratio:           v.Ratio,
			Note:            v.Note,
			Warning:         v.Warning,
		}
		// 多档位比较才输出档位子判定，单档位保持旧 JSON 形状
		if len(v.Buckets) > 1 {
			for _, bv := range v.Buckets {
				jp.Buckets = append(jp.Buckets, jsonBucket{
					ContextBucket:   bv.ContextBucket,
					Verdict:         bv.Verdict,
					Statistic:       bv.Statistic,
					Threshold:       bv.Threshold,
					ThresholdSource: bv.ThresholdSource,
					Ratio:           bv.Ratio,
					Note:            bv.Note,
					Warning:         bv.Warning,
				})
			}
		}
		for _, c := range v.Cells {
			jp.Cells = append(jp.Cells, jsonCell{
				Key: c.Key, NA: c.NA, NB: c.NB,
				Stat: c.Stat, Insufficient: c.Insufficient, Extra: c.Extra,
			})
		}
		rep.Probes = append(rep.Probes, jp)
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rep); err != nil {
		return fmt.Errorf("JSON 报告写入失败: %w", err)
	}
	return nil
}

func toSide(m compare.TransportMetrics) jsonTransportSide {
	return jsonTransportSide{
		Availability: m.Availability,
		ErrorRate:    m.ErrorRate,
		TimeoutRate:  m.TimeoutRate,
		LatencyMs:    jsonPercentiles{m.LatencyMs.P50, m.LatencyMs.P90, m.LatencyMs.P99},
		TtftMs:       jsonPercentiles{m.TtftMs.P50, m.TtftMs.P90, m.TtftMs.P99},
		Tps:          jsonPercentiles{m.Tps.P50, m.Tps.P90, m.Tps.P99},
	}
}

// histOrNil 空直方图输出 null（与 Percentiles 的 null 语义一致：
// 下游能区分「没样本」和「样本都挤在一个 bin」）。
func histOrNil(h stats.Hist) *stats.Hist {
	if h.Empty() {
		return nil
	}
	return &h
}
