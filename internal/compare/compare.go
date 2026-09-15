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
	PlanDigest string // 严格闸门下两侧一致；子集模式下为 A 侧 digest（Scope 给出两侧）
	Planned    int    // 计划比较的探针数（子集模式下为交集探针数）
	Compared   int    // 实际算出统计量的探针数
	Coverage   float64
	Verdicts   []probe.Verdict
	Notes      []string // 提示性信息：sampling 差异、数据不完整等

	// Scope 子集模式（--allow-subset）下的比较范围：digest 一致时为 nil。
	// 报告层据此强制渲染「比较范围」段落，防止缩水的交集被当成完整比较消费。
	Scope *Scope

	TransportA, TransportB   TransportMetrics
	IncompleteA, IncompleteB bool // 缺 aggregates 行 = 该侧采集未完成

	// Hists 传输指标的两侧联合直方图（描述性证据，不参与判定）。
	// 与分位数同源：样本取自两侧全部 record（aggregates 无逐样本数据）。
	Hists TransportHists
}

// Scope 子集模式的比较范围描述（DATAFLOW §3.2）。
type Scope struct {
	Mode     string           `json:"mode"` // "subset"
	DigestA  string           `json:"plan_digest_a"`
	DigestB  string           `json:"plan_digest_b"`
	Probes   []string         `json:"probes"`
	MinN     map[string]int   `json:"min_n"` // 生效的单侧样本下限及来源见 Notes
	Repeats  map[string]int   `json:"repeats"`
	Excluded []ScopeExclusion `json:"excluded,omitempty"`
	Notes    []string         `json:"notes,omitempty"`
}

// Options 比较选项。
type Options struct {
	// AllowSubset 允许子集比较：plan_digest 不等时按字段三分法调和
	// （strict 冲突或交集为空仍拒绝），而非一律拒绝。默认 false。
	AllowSubset bool
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

// Run 执行完整比较流程（严格闸门）：闸门 → 阈值 → 配对 → 逐探针判定 → 汇总。
// 闸门失败返回 *GateError；缺阈值返回 config.MissingThresholdError。
func Run(a, b *rawdata.File, flagThresholds map[string]float64, cfg *config.File) (*Result, error) {
	return RunWith(a, b, flagThresholds, cfg, Options{})
}

// RunWith 执行完整比较流程，可开子集模式：闸门 → 阈值 → 配对 → 逐探针判定 → 汇总。
// 闸门失败返回 *GateError；缺阈值返回 config.MissingThresholdError。
func RunWith(a, b *rawdata.File, flagThresholds map[string]float64, cfg *config.File, opts Options) (*Result, error) {
	// 1. 可比性闸门：严格模式任何计划差异一律拒绝；子集模式按字段三分法调和
	rec, err := Gate(a, b, opts.AllowSubset)
	if err != nil {
		return nil, err
	}

	// 2. 计划探针（排序保证确定性），查注册表分出已实现 / 未实现
	probeIDs := rec.ProbeIDs

	pValueProbes := map[string]bool{}
	// probeBuckets：已实现探针 → 按档位判定时需要阈值的 context_buckets。
	// cell_key 不含 context_bucket 维度（旧 rawData / 自定义计划）时为空，
	// 走单阈值退化路径。子集模式下这里是两侧声明档位的交集。
	probeBuckets := map[string][]int{}
	for _, id := range probeIDs {
		if p, ok := probe.Get(id); ok {
			if p.Meta().StatKind == probe.StatPValue {
				pValueProbes[id] = true
			}
			probeBuckets[id] = nil
			if BucketedCellKey(rec.Probes[id].CellKey) {
				probeBuckets[id] = rec.Probes[id].ContextBuckets
			}
		}
	}

	// 3. 阈值闸门：每个待比较（已实现）探针的每个档位必须有用户阈值，
	// 档位专用 > 探针级兜底；同级内 flag > config
	thresholds, err := config.ResolveThresholds(probeBuckets, flagThresholds, cfg, pValueProbes)
	if err != nil {
		return nil, err
	}

	res := &Result{
		PlanDigest:  rec.DigestA,
		Planned:     len(probeIDs),
		IncompleteA: a.Aggregates == nil,
		IncompleteB: b.Aggregates == nil,
		TransportA:  transportOf(a),
		TransportB:  transportOf(b),
		Hists:       transportHists(a.Records, b.Records),
	}
	if !rec.Identical {
		res.Scope = buildScope(rec, cfg)
		res.Notes = append(res.Notes, scopeNotes(rec)...)
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

	// 4. 逐探针：配对 → 按 context_bucket 分区 → 逐档位 Compare → rollup
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
		pairs := pairCells(a, b, id, rec.Probes[id].CellKey, !rec.Identical)
		if !rec.Identical && len(probeBuckets[id]) > 0 {
			pairs = filterPairsByBuckets(pairs, probeBuckets[id])
		}
		minN := resolveMinN(cfg, id, rec.Probes[id], rec.BProbes[id], p)
		var v probe.Verdict
		if len(probeBuckets[id]) > 1 {
			v = compareBucketed(p, pairs, thresholds[id], minN)
		} else {
			// 单档位（或 cell_key 不含档位维度）：不分区，探针级阈值。
			// 档位取计划里的唯一 bucket，保证档位专用阈值（如 onetoken.8000）生效
			bucket := 0
			if bs := probeBuckets[id]; len(bs) == 1 {
				bucket = bs[0]
			}
			th, _ := thresholds[id].For(bucket)
			v = p.Compare(pairs, th.Value, minN)
			v.ThresholdSource = th.Source
		}
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

// resolveMinN 解析探针生效的单侧样本下限。min_n 是判据参数而非采集参数
// （与 thresholds 同类，不进可比性闸门），优先级：
// 本地 config（probes.<id>.min_n）> 计划烘焙值（A 侧，缺则 B 侧）> DefaultMinN > 探针默认。
// repeats 取小后使用者常需要下调 min_n，本地 config 必须能覆盖两侧计划里的旧值。
func resolveMinN(cfg *config.File, id string, planA, planB rawdata.ProbePlan, p probe.Probe) int {
	if cfg != nil {
		if pc, ok := cfg.Probes[id]; ok && pc.MinN != nil && *pc.MinN >= 1 {
			return *pc.MinN
		}
	}
	if planA.MinN >= 1 {
		return planA.MinN
	}
	if planB.MinN >= 1 {
		return planB.MinN
	}
	if v, ok := config.DefaultMinN[id]; ok {
		return v
	}
	return p.Meta().MinN
}

// buildScope 由调和结果构造报告用的比较范围。min_n 展示生效值
// （与判定同源：config > plan > default）。
func buildScope(rec *Reconciled, cfg *config.File) *Scope {
	sc := &Scope{
		Mode:     "subset",
		DigestA:  rec.DigestA,
		DigestB:  rec.DigestB,
		Probes:   append([]string(nil), rec.ProbeIDs...),
		MinN:     map[string]int{},
		Repeats:  map[string]int{},
		Excluded: rec.Excluded,
		Notes:    rec.Notes,
	}
	for _, id := range rec.ProbeIDs {
		pp := rec.Probes[id]
		if p, ok := probe.Get(id); ok {
			sc.MinN[id] = resolveMinN(cfg, id, pp, rec.BProbes[id], p)
		}
		sc.Repeats[id] = pp.Repeats
	}
	return sc
}

// scopeNotes 把调和结果渲染成人读的 NOTE 行（stdout 报告与 JSON notes 共用）。
func scopeNotes(rec *Reconciled) []string {
	var notes []string
	notes = append(notes, "子集比较（--allow-subset）：两侧采集计划不同，仅比较可调和的交集部分")
	for _, ex := range rec.Excluded {
		if ex.Kind == "bucket" && ex.Bucket != nil {
			notes = append(notes, fmt.Sprintf("排除档位 %s/context_bucket=%d：%s", ex.ProbeID, *ex.Bucket, ex.Reason))
		} else if ex.Kind == "probe" {
			notes = append(notes, fmt.Sprintf("排除探针 %s：%s", ex.ProbeID, ex.Reason))
		}
	}
	notes = append(notes, rec.Notes...)
	return notes
}

// filterPairsByBuckets 子集模式下把配对 cell 限制在生效档位集合内。
// 防御性过滤：正常情况单侧档位本就没有对侧 record、不会成对，
// 但计划与实际数据不一致的 rawData（手工构造/续跑残留）不该把
// 交集之外的档位混进统计。
func filterPairsByBuckets(pairs []probe.CellPair, buckets []int) []probe.CellPair {
	keep := make([]probe.CellPair, 0, len(pairs))
	for _, pr := range pairs {
		b, err := strconv.Atoi(pr.Key["context_bucket"])
		if err != nil {
			continue
		}
		found := false
		for _, x := range buckets {
			if x == b {
				found = true
				break
			}
		}
		if found {
			keep = append(keep, pr)
		}
	}
	return keep
}

// BucketedCellKey 判断计划的 cell_key 是否含 context_bucket 维度。
// 含档位维度的计划按档位分区判定（要求档位级阈值齐全）；
// 不含的（旧 rawData / 自定义计划）走单阈值退化路径。
// collect 侧的阈值预检复用本函数，保证与比较侧同构。
func BucketedCellKey(cellKey []string) bool {
	if len(cellKey) == 0 {
		cellKey = []string{"question_id", "context_bucket"} // pairCells 的默认
	}
	for _, k := range cellKey {
		if k == "context_bucket" {
			return true
		}
	}
	return false
}

// compareBucketed 按 context_bucket 分区，逐档位用各自阈值判定，
// 再收敛出探针级 rollup：
//   - Statistic/Threshold/ThresholdSource/Ratio 取最差 ratio 档位的值，
//     保持 ratio > 1 ⟺ fail 的不变式；
//   - inconclusive 档位排除、不污染整体，进 rollup 的 Warning 点名；
//     全部档位 inconclusive 才整体 inconclusive；
//   - Cells 拼接各档位，Key 自带 context_bucket，明细零丢失。
func compareBucketed(p probe.Probe, pairs []probe.CellPair, pt config.ProbeThresholds, minN int) probe.Verdict {
	byBucket := map[int][]probe.CellPair{}
	for _, pr := range pairs {
		b, err := strconv.Atoi(pr.Key["context_bucket"])
		if err != nil {
			b = -1 // 理论不可达（cell_key 含 context_bucket 即数字）；归入未知分区
		}
		byBucket[b] = append(byBucket[b], pr)
	}
	buckets := make([]int, 0, len(byBucket))
	for b := range byBucket {
		buckets = append(buckets, b)
	}
	sort.Ints(buckets)

	roll := probe.Verdict{ProbeID: p.Meta().ID}
	var (
		judged  []probe.BucketVerdict
		skipped []string // inconclusive 档位的说明
		worst   *probe.BucketVerdict
	)
	for _, b := range buckets {
		th, ok := pt.For(b)
		if !ok {
			// ResolveThresholds 已保证不缺；防御性处理
			skipped = append(skipped, fmt.Sprintf("bucket %d 无阈值", b))
			continue
		}
		sub := p.Compare(byBucket[b], th.Value, minN)
		bv := probe.BucketVerdict{
			ContextBucket:   b,
			Verdict:         sub.Verdict,
			Statistic:       sub.Statistic,
			Threshold:       th.Value,
			ThresholdSource: th.Source,
			Ratio:           sub.Ratio,
			Note:            sub.Note,
			Warning:         sub.Warning,
		}
		roll.Buckets = append(roll.Buckets, bv)
		roll.Cells = append(roll.Cells, sub.Cells...)
		if sub.Verdict == "inconclusive" {
			note := sub.Note
			if note == "" {
				note = "样本不足或观测不可用"
			}
			skipped = append(skipped, fmt.Sprintf("bucket %d: %s", b, note))
			continue
		}
		judged = append(judged, bv)
		if worst == nil || worseThan(bv, *worst, p.Meta().StatKind) {
			cp := bv
			worst = &cp
		}
	}

	if len(judged) == 0 {
		roll.Verdict = "inconclusive"
		if len(pairs) == 0 {
			roll.Note = "两侧没有共同 cell"
		} else {
			roll.Note = "所有档位均无法判定"
		}
		if len(skipped) > 0 {
			roll.Note += "（" + strings.Join(skipped, "; ") + "）"
		}
		return roll
	}

	roll.Verdict = worst.Verdict
	roll.Statistic = worst.Statistic
	roll.Threshold = worst.Threshold
	roll.ThresholdSource = worst.ThresholdSource
	roll.Ratio = worst.Ratio
	if len(judged) > 1 {
		roll.Note = fmt.Sprintf("%d 个档位各自判定，取最差（bucket %d）", len(judged), worst.ContextBucket)
	} else {
		roll.Note = worst.Note
	}
	// 警告聚合：逐档位功效警告都要保留（多档位下 lowPower 更易触发）
	var warns []string
	for _, bv := range judged {
		if bv.Warning != "" {
			warns = append(warns, fmt.Sprintf("bucket %d: %s", bv.ContextBucket, bv.Warning))
		}
	}
	for _, s := range skipped {
		warns = append(warns, "档位未判定: "+s)
	}
	roll.Warning = strings.Join(warns, "; ")
	return roll
}

// worseThan 判定 a 是否比 b 更差：优先按 ratio（>1 恒等价 fail），
// ratio 缺失时按 verdict 等级（fail > pass），再按 StatKind 方向比统计量。
func worseThan(a, b probe.BucketVerdict, kind probe.StatKind) bool {
	if a.Ratio != nil && b.Ratio != nil {
		return *a.Ratio > *b.Ratio
	}
	if a.Ratio != nil {
		return true
	}
	if b.Ratio != nil {
		return false
	}
	rank := func(v string) int {
		switch v {
		case "fail":
			return 2
		case "pass":
			return 1
		}
		return 0
	}
	if rank(a.Verdict) != rank(b.Verdict) {
		return rank(a.Verdict) > rank(b.Verdict)
	}
	if a.Statistic == nil || b.Statistic == nil {
		return false
	}
	if kind == probe.StatPValue {
		return *a.Statistic < *b.Statistic // p 越小越差
	}
	return *a.Statistic > *b.Statistic // 距离越大越差
}

// pairCells 把两侧 record 按 cell_key 分组（仅该探针且 status=success），
// 返回两侧都存在的配对 cell，按键排序保证确定性。
// subset 为 true（子集模式）时，每个 cell 两侧按 repeat_index 升序
// 截断到相同条数 min(nA, nB)：repeats 取小后，JSD 类统计量（onetoken /
// toolcall）对两侧样本量不对称敏感（小样本系统性抬高 JSD），降采样让
// 已标定的阈值继续成立。取前 k 个 repeat_index 而非随机抽样，保证确定性。
func pairCells(a, b *rawdata.File, probeID string, cellKey []string, subset bool) []probe.CellPair {
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
		ga, gb := groupA[k], groupB[k]
		obsA, obsB := ga.obs, gb.obs
		if subset {
			n := min(len(obsA), len(obsB))
			obsA = downsample(ga.items, n)
			obsB = downsample(gb.items, n)
		}
		pairs = append(pairs, probe.CellPair{
			Key: ga.key,
			A:   obsA,
			B:   obsB,
		})
	}
	return pairs
}

// downsample 按 repeat_index 升序稳定排序后取前 n 条观测。
// 同 repeat_index 的多条 record（重试后重复落盘等）按原文件顺序保持稳定。
func downsample(items []cellItem, n int) []probe.Observation {
	sorted := make([]cellItem, len(items))
	copy(sorted, items)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].repeat < sorted[j].repeat })
	if n > len(sorted) {
		n = len(sorted)
	}
	out := make([]probe.Observation, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, sorted[i].obs)
	}
	return out
}

// cellItem 一条参与配对的观测及其 repeat_index（降采样排序用）。
type cellItem struct {
	obs    probe.Observation
	repeat int
}

// cellGroup 一个 cell 的键分量与观测集合。
type cellGroup struct {
	key   map[string]string
	obs   []probe.Observation
	items []cellItem
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
		g.items = append(g.items, cellItem{obs: r.Observation, repeat: r.RepeatIndex})
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
