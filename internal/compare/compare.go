package compare

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/chaterm/token-verifier/internal/config"
	"github.com/chaterm/token-verifier/internal/probe"
	"github.com/chaterm/token-verifier/internal/rawdata"
	"github.com/chaterm/token-verifier/internal/stats"
)

// Result 一次比较的完整结果，供报告层渲染。
type Result struct {
	PlanDigest string // 两侧一致（闸门已保证）
	Planned    int    // 计划比较的探针数
	Compared   int    // 实际算出统计量的探针数
	Coverage   float64
	Verdicts   []probe.Verdict
	Notes      []string // 提示性信息：sampling 差异、数据不完整等

	TransportA, TransportB   TransportMetrics
	IncompleteA, IncompleteB bool // 缺 aggregates 行 = 该侧采集未完成

	// Hists 传输指标的两侧联合直方图（描述性证据，不参与判定）。
	// 与分位数同源：样本取自两侧全部 record（aggregates 无逐样本数据）。
	Hists TransportHists
}

// TransportHists 三个传输指标的联合直方图：A/B 共享分箱边界，
// counts 逐 bin 对齐，报告层直接渲染成两侧分布对比。
type TransportHists struct {
	LatencyMs stats.Hist
	TtftMs    stats.Hist
	Tps       stats.Hist
}

// transportHistBins 传输直方图的 bin 数：够看出分布形状与分离，
// 又不至于在终端渲染时占满屏幕。
const transportHistBins = 20

// TransportMetrics 一侧的描述性传输指标（不参与判定，DATAFLOW §3.5）。
// 指针字段：nil = 该指标不可用（无样本），JSON 输出为 null。
type TransportMetrics struct {
	Availability float64
	ErrorRate    float64
	TimeoutRate  float64
	LatencyMs    Percentiles
	TtftMs       Percentiles
	Tps          Percentiles
}

// Percentiles 分位数三元组。
type Percentiles struct {
	P50, P90, P99 *float64
}

// Run 执行完整比较流程：闸门 → 阈值 → 配对 → 逐探针判定 → 汇总。
// 闸门失败返回 *GateError；缺阈值返回 config.MissingThresholdError。
func Run(a, b *rawdata.File, flagThresholds map[string]float64, cfg *config.File) (*Result, error) {
	// 1. digest 闸门：任何计划差异一律拒绝，不做部分比较
	if err := CheckGate(a, b); err != nil {
		return nil, err
	}

	plan, err := a.Manifest.CollectionPlanParsed()
	if err != nil {
		return nil, fmt.Errorf("解析采集计划失败: %w", err)
	}

	// 2. 计划探针（排序保证确定性），查注册表分出已实现 / 未实现
	probeIDs := make([]string, 0, len(plan.Probes))
	for id := range plan.Probes {
		probeIDs = append(probeIDs, id)
	}
	sort.Strings(probeIDs)

	var implemented []string
	pValueProbes := map[string]bool{}
	for _, id := range probeIDs {
		if p, ok := probe.Get(id); ok {
			implemented = append(implemented, id)
			if p.Meta().StatKind == probe.StatPValue {
				pValueProbes[id] = true
			}
		}
	}

	// 3. 阈值闸门：每个待比较（已实现）探针必须有用户阈值，flag > config
	thresholds, err := config.ResolveThresholds(implemented, flagThresholds, cfg, pValueProbes)
	if err != nil {
		return nil, err
	}

	res := &Result{
		PlanDigest:  a.Manifest.PlanDigest,
		Planned:     len(probeIDs),
		IncompleteA: a.Aggregates == nil,
		IncompleteB: b.Aggregates == nil,
		TransportA:  transportOf(a),
		TransportB:  transportOf(b),
		Hists:       transportHists(a.Records, b.Records),
	}
	if res.IncompleteA {
		res.Notes = append(res.Notes, "A 侧 rawData 缺 aggregates 行（采集未完成），传输指标由 record 重算")
	}
	if res.IncompleteB {
		res.Notes = append(res.Notes, "B 侧 rawData 缺 aggregates 行（采集未完成），传输指标由 record 重算")
	}
	if a.Truncated {
		res.Notes = append(res.Notes, "A 侧 rawData 尾部 gzip 成员不完整（采集中断），已读到的 record 仍参与比较")
	}
	if b.Truncated {
		res.Notes = append(res.Notes, "B 侧 rawData 尾部 gzip 成员不完整（采集中断），已读到的 record 仍参与比较")
	}
	res.Notes = append(res.Notes, samplingNotes(a, b)...)

	// 4. 逐探针：配对 → Compare
	for _, id := range probeIDs {
		p, ok := probe.Get(id)
		if !ok {
			res.Verdicts = append(res.Verdicts, probe.Verdict{
				ProbeID: id,
				Verdict: "inconclusive",
				Note:    "本版本尚未实现该探针",
			})
			continue
		}
		th := thresholds[id]
		pairs := pairCells(a, b, id, plan.Probes[id].CellKey)
		v := p.Compare(pairs, th.Value, plan.Probes[id].MinN)
		v.ThresholdSource = th.Source
		if v.Verdict == "inconclusive" {
			v.Note = strings.Join(append([]string{v.Note}, skippedNotes(id, a, b)...), "; ")
		}
		res.Verdicts = append(res.Verdicts, v)
		if v.Verdict == "pass" || v.Verdict == "fail" {
			res.Compared++
		}
	}
	if res.Planned > 0 {
		res.Coverage = float64(res.Compared) / float64(res.Planned)
	}
	return res, nil
}

// pairCells 把两侧 record 按 cell_key 分组（仅该探针且 status=success），
// 返回两侧都存在的配对 cell，按键排序保证确定性。
func pairCells(a, b *rawdata.File, probeID string, cellKey []string) []probe.CellPair {
	if len(cellKey) == 0 {
		cellKey = []string{"question_id", "context_bucket"}
	}
	groupA := groupByCell(a.Records, probeID, cellKey)
	groupB := groupByCell(b.Records, probeID, cellKey)

	joined := make([]string, 0, len(groupA))
	for k := range groupA {
		if _, ok := groupB[k]; ok {
			joined = append(joined, k)
		}
	}
	sort.Strings(joined)

	pairs := make([]probe.CellPair, 0, len(joined))
	for _, k := range joined {
		pairs = append(pairs, probe.CellPair{
			Key: groupA[k].key,
			A:   groupA[k].obs,
			B:   groupB[k].obs,
		})
	}
	return pairs
}

// cellGroup 一个 cell 的键分量与观测集合。
type cellGroup struct {
	key map[string]string
	obs []probe.Observation
}

// groupByCell 把某探针的 record 按 cell_key 分组；其他探针与键值提取失败的 record 跳过。
func groupByCell(records []rawdata.Record, probeID string, cellKey []string) map[string]*cellGroup {
	groups := map[string]*cellGroup{}
	for i := range records {
		r := &records[i]
		if r.ProbeID != probeID || r.Status != "success" {
			continue
		}
		key, joined, ok := buildCellKey(r, cellKey)
		if !ok {
			continue
		}
		g, has := groups[joined]
		if !has {
			g = &cellGroup{key: key}
			groups[joined] = g
		}
		g.obs = append(g.obs, r.Observation)
	}
	return groups
}

// buildCellKey 提取 record 的 cell_key 各分量，返回（分量表，连接键）。
func buildCellKey(r *rawdata.Record, cellKey []string) (map[string]string, string, bool) {
	key := make(map[string]string, len(cellKey))
	parts := make([]string, 0, len(cellKey))
	for _, name := range cellKey {
		var v string
		switch name {
		case "question_id":
			v = r.QuestionID
		case "context_bucket":
			v = strconv.Itoa(r.ContextBucket)
		case "protocol":
			v = r.Protocol
		case "transport_mode":
			v = r.TransportMode
		case "thinking_effort":
			if r.ThinkingEffort == nil {
				v = "null"
			} else {
				v = *r.ThinkingEffort
			}
		default:
			return nil, "", false // 未知分组键：该 record 不参与配对
		}
		key[name] = v
		parts = append(parts, v)
	}
	return key, strings.Join(parts, "|"), true
}

// transportOf 提取一侧的传输指标：优先用 aggregates 里的现成值；
// aggregates 缺失或不含 transport 数据时从 record 重算（aggregates 是派生数据，以 record 为准）。
func transportOf(f *rawdata.File) TransportMetrics {
	if f.Aggregates != nil && f.Aggregates.Transport.Overall.LatencyMs != nil {
		o := f.Aggregates.Transport.Overall
		return TransportMetrics{
			Availability: o.Availability,
			ErrorRate:    o.ErrorRate,
			TimeoutRate:  o.TimeoutRate,
			LatencyMs:    fromMap(o.LatencyMs),
			TtftMs:       fromMap(o.TtftMs),
			Tps:          fromMap(o.Tps),
		}
	}
	return recomputeTransport(f.Records)
}

// fromMap 把 aggregates 里的分位数 map 转为 Percentiles。
func fromMap(m map[string]float64) Percentiles {
	get := func(k string) *float64 {
		if v, ok := m[k]; ok {
			return &v
		}
		return nil
	}
	return Percentiles{P50: get("p50"), P90: get("p90"), P99: get("p99")}
}

// recomputeTransport 从 record 重算总体传输指标（aggregates 缺失时的回退）。
func recomputeTransport(records []rawdata.Record) TransportMetrics {
	n := len(records)
	if n == 0 {
		return TransportMetrics{}
	}
	var ok, timeout int
	var latency, ttft, tps []float64
	for i := range records {
		r := &records[i]
		if r.Status == "success" {
			ok++
		}
		if r.ErrorKind != nil && *r.ErrorKind == "timeout" {
			timeout++
		}
		latency = append(latency, float64(r.LatencyMs))
		if r.TtftMs != nil {
			ttft = append(ttft, float64(*r.TtftMs))
		}
		if r.Tps != nil {
			tps = append(tps, *r.Tps)
		}
	}
	return TransportMetrics{
		Availability: float64(ok) / float64(n),
		ErrorRate:    float64(n-ok) / float64(n),
		TimeoutRate:  float64(timeout) / float64(n),
		LatencyMs:    quantiles(latency),
		TtftMs:       quantiles(ttft),
		Tps:          quantiles(tps),
	}
}

// quantiles 算 p50/p90/p99；无样本返回全 nil。
func quantiles(xs []float64) Percentiles {
	if len(xs) == 0 {
		return Percentiles{}
	}
	q := func(p float64) *float64 {
		v := stats.Quantile(xs, p)
		if math.IsNaN(v) {
			return nil
		}
		return &v
	}
	return Percentiles{P50: q(0.5), P90: q(0.9), P99: q(0.99)}
}

// transportHists 从两侧 record 提取传输指标样本，逐指标建联合直方图。
// 描述性证据：与分位数一样不参与判定，但给出「形状」层面的对比 ——
// p50/p90 相同的两侧，分布可能一个是窄峰一个是双峰。
func transportHists(a, b []rawdata.Record) TransportHists {
	la, lta, ta := transportSamples(a)
	lb, ltb, tb := transportSamples(b)
	return TransportHists{
		LatencyMs: stats.JointHist(la, lb, transportHistBins),
		TtftMs:    stats.JointHist(lta, ltb, transportHistBins),
		Tps:       stats.JointHist(ta, tb, transportHistBins),
	}
}

// transportSamples 一侧 record 的 (latency, ttft, tps) 样本。
// ttft/tps 可缺失（非流式无 ttft）；latency 每条 record 都有。
func transportSamples(records []rawdata.Record) (latency, ttft, tps []float64) {
	for i := range records {
		r := &records[i]
		latency = append(latency, float64(r.LatencyMs))
		if r.TtftMs != nil {
			ttft = append(ttft, float64(*r.TtftMs))
		}
		if r.Tps != nil {
			tps = append(tps, *r.Tps)
		}
	}
	return latency, ttft, tps
}

// samplingNotes 两侧 sampling 分配不同时输出提示性 NOTE（不是拒绝，DATAFLOW §3.2）。
func samplingNotes(a, b *rawdata.File) []string {
	if sameRawJSON(a.Manifest.Sampling, b.Manifest.Sampling) {
		return nil
	}
	return []string{"两侧的协议/传输分配（sampling）不同：观测差异可能来自请求格式而非模型本身"}
}

// skippedNotes 汇总两侧 manifest 里该探针的跳过记录，解释数据缺失的原因。
func skippedNotes(probeID string, a, b *rawdata.File) []string {
	var notes []string
	for side, f := range [2]*rawdata.File{a, b} {
		label := "A"
		if side == 1 {
			label = "B"
		}
		for _, s := range f.Manifest.Skipped {
			if s.ProbeID == probeID {
				notes = append(notes, fmt.Sprintf("%s 侧跳过: %s/%s (%s)", label, s.ProbeID, s.Protocol, s.Reason))
			}
		}
	}
	return notes
}

// sameRawJSON 判断两段原始 JSON 是否语义相等。
func sameRawJSON(a, b json.RawMessage) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return false
	}
	return sameJSON(av, bv)
}
