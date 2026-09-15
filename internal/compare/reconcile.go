// reconcile.go：子集模式（--allow-subset）下的采集计划调和。
//
// 严格闸门（CheckGate）要求 plan_digest 逐字节一致；子集模式把它放宽为
// 字段级三分法（SPEC-RAWDATA §digest 闸门 / DATAFLOW §3.2）：
//
//	strict    —— 不等即拒绝，子集模式也不放行：
//	             plan 层：suite_version / padding_algo_version /
//	                     padding_seed / normalize / output_contract
//	             探针层：probe_version / observation_schema / normalize_rule /
//	                     cell_key / question_ids /
//	                     temperature / top_p / max_tokens / thinking_effort
//	intersect —— context_buckets：按两侧声明的档位取交集；无交集则该探针剔除
//	relaxed   —— repeats：取两侧较小值，cell 内按 repeat_index 降采样对齐
//	             min_n：闸门忽略（判据参数而非采集参数，与 thresholds 同类），
//	                    比较时按 本地 config > 计划烘焙值 > 探针默认 解析
//
// question_set_digest 例外：它是派生字段（全部启用探针 question_ids 并集的
// 哈希），两侧启用探针集合不同时必然不等 —— 这正是子集模式要容忍的差异。
// 真实题库差异由逐探针的 question_ids strict 比对拦截；它不等只出 NOTE。
//
// 探针集合本身也按交集处理：disabled 探针不进计划（plan.Build 只遍历
// EnabledProbes），所以「两侧都存在的探针」天然等价于「两侧都 enabled」。
package compare

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/chaterm/token-verifier/internal/rawdata"
)

// ScopeExclusion 子集模式下被排除的探针或档位。
type ScopeExclusion struct {
	Kind    string `json:"kind"` // "probe" | "bucket"
	ProbeID string `json:"probe_id"`
	Bucket  *int   `json:"bucket,omitempty"` // Kind == "bucket" 时有值
	Reason  string `json:"reason"`
}

// Reconciled 两侧采集计划的调和结果。Identical 为 true 时（digest 相等）
// Probes 即 A 侧计划原样，语义与严格闸门完全一致。
type Reconciled struct {
	Identical bool
	// Probes 参与比较的探针 → 生效计划（buckets 已取交集、repeats 已取小）。
	Probes map[string]rawdata.ProbePlan
	// BProbes B 侧原始计划块，供 min_n 回退与报告。
	BProbes map[string]rawdata.ProbePlan
	// ProbeIDs 参与比较的探针，排序保证确定性。
	ProbeIDs []string
	Excluded []ScopeExclusion
	// Notes 放宽项说明（repeats 取小、min_n 两侧不一致等），进报告。
	Notes []string
	// DigestA / DigestB 两侧 plan_digest（Identical 时相等）。
	DigestA, DigestB string
}

// ReconcilePlans 调和两侧 manifest 里的 collection_plan 原始 JSON。
// 返回 (调和结果, strict 冲突列表)；strict 冲突非空或交集无探针时不可比较，
// 由调用方包装成 *GateError。JSON 解析失败返回 error。
func ReconcilePlans(rawA, rawB json.RawMessage, digestA, digestB string) (*Reconciled, []FieldDiff, error) {
	planA, err := parsePlanMirror(rawA)
	if err != nil {
		return nil, nil, fmt.Errorf("A 侧采集计划解析失败: %w", err)
	}
	planB, err := parsePlanMirror(rawB)
	if err != nil {
		return nil, nil, fmt.Errorf("B 侧采集计划解析失败: %w", err)
	}

	rec := &Reconciled{
		Probes:  map[string]rawdata.ProbePlan{},
		BProbes: map[string]rawdata.ProbePlan{},
		DigestA: digestA,
		DigestB: digestB,
	}

	// 1. plan 层（probes 之外）严格相等：对原始 JSON 去掉 probes 后泛化 diff，
	//    normalize / output_contract 等镜像结构体没有的字段也被覆盖。
	//    question_set_digest 例外：它是派生字段 —— 全部启用探针 question_ids
	//    并集的哈希（plan.Build），两侧启用探针集合不同时必然不等，而这正是
	//    子集模式要容忍的差异（探针交集已单独处理）。逐探针的 question_ids
	//    仍严格比对，真实题库差异在那里被拦截；这里不等只降级为 NOTE。
	var conflicts []FieldDiff
	conflicts = append(conflicts, planLevelDiffs(rawA, rawB)...)
	if planA.QuestionSetDigest != planB.QuestionSetDigest {
		rec.Notes = append(rec.Notes, fmt.Sprintf(
			"两侧 question_set_digest 不同（%s vs %s）：该值由启用探针的题目并集派生，"+
				"差异来自启用探针集合不同；参与比较的探针已逐一严格校验 question_ids",
			truncDigestNote(planA.QuestionSetDigest), truncDigestNote(planB.QuestionSetDigest)))
	}

	// 2. 探针集合取交集；单侧独有的探针剔除并记录原因
	aIDs := sortedProbeIDs(planA.Probes)
	bIDs := sortedProbeIDs(planB.Probes)
	inB := map[string]bool{}
	for _, id := range bIDs {
		inB[id] = true
	}
	inA := map[string]bool{}
	for _, id := range aIDs {
		inA[id] = true
	}
	for _, id := range aIDs {
		if !inB[id] {
			rec.Excluded = append(rec.Excluded, ScopeExclusion{
				Kind: "probe", ProbeID: id, Reason: "仅 A 侧计划包含该探针",
			})
		}
	}
	for _, id := range bIDs {
		if !inA[id] {
			rec.Excluded = append(rec.Excluded, ScopeExclusion{
				Kind: "probe", ProbeID: id, Reason: "仅 B 侧计划包含该探针",
			})
		}
	}

	// 3. 共同探针：strict 字段逐一比对；buckets 取交集；repeats 取小
	for _, id := range aIDs {
		if !inB[id] {
			continue
		}
		pa, pb := planA.Probes[id], planB.Probes[id]
		rec.BProbes[id] = pb

		conflicts = append(conflicts, strictProbeDiffs(id, &pa, &pb)...)

		eff := pa
		// context_buckets：按声明值取交集（保持升序）。两侧同为空（旧 rawData /
		// cell_key 不含档位维度）视为相等，走退化路径。
		if !intsEqual(pa.ContextBuckets, pb.ContextBuckets) {
			inter := intersectInts(pa.ContextBuckets, pb.ContextBuckets)
			for _, b := range pa.ContextBuckets {
				if !intInSlice(b, inter) {
					bv := b
					rec.Excluded = append(rec.Excluded, ScopeExclusion{
						Kind: "bucket", ProbeID: id, Bucket: &bv,
						Reason: "仅 A 侧计划包含该档位",
					})
				}
			}
			for _, b := range pb.ContextBuckets {
				if !intInSlice(b, inter) {
					bv := b
					rec.Excluded = append(rec.Excluded, ScopeExclusion{
						Kind: "bucket", ProbeID: id, Bucket: &bv,
						Reason: "仅 B 侧计划包含该档位",
					})
				}
			}
			if len(inter) == 0 {
				rec.Excluded = append(rec.Excluded, ScopeExclusion{
					Kind: "probe", ProbeID: id,
					Reason: fmt.Sprintf("两侧 context_buckets 无交集（%v vs %v）",
						pa.ContextBuckets, pb.ContextBuckets),
				})
				continue
			}
			eff.ContextBuckets = inter
		}
		// repeats：取小。cell 内实际对齐由 pairCells 按 repeat_index 降采样完成，
		// 这里只负责报告与生效值。
		if pb.Repeats < eff.Repeats {
			eff.Repeats = pb.Repeats
		}
		if pa.Repeats != pb.Repeats {
			rec.Notes = append(rec.Notes, fmt.Sprintf(
				"探针 %s repeats 取两侧较小值 %d（A=%d, B=%d），cell 内按 repeat_index 降采样对齐",
				id, eff.Repeats, pa.Repeats, pb.Repeats))
		}
		// min_n 不参与调和：闸门忽略，比较侧按 config > plan > default 解析
		if pa.MinN != pb.MinN {
			rec.Notes = append(rec.Notes, fmt.Sprintf(
				"探针 %s 两侧计划烘焙的 min_n 不一致（A=%d, B=%d），生效值由比较侧解析（config > plan > default）",
				id, pa.MinN, pb.MinN))
		}

		rec.Probes[id] = eff
		rec.ProbeIDs = append(rec.ProbeIDs, id)
	}
	sort.Strings(rec.ProbeIDs)
	sort.Slice(rec.Excluded, func(i, j int) bool {
		if rec.Excluded[i].ProbeID != rec.Excluded[j].ProbeID {
			return rec.Excluded[i].ProbeID < rec.Excluded[j].ProbeID
		}
		return rec.Excluded[i].Kind < rec.Excluded[j].Kind
	})

	if len(conflicts) > 0 {
		sort.Slice(conflicts, func(i, j int) bool { return conflicts[i].Path < conflicts[j].Path })
		return rec, conflicts, nil
	}
	return rec, nil, nil
}

// ReconciledFromPlan 严格闸门通过后由 A 侧计划直接构造调和结果（Identical）。
func ReconciledFromPlan(plan *rawdata.CollectionPlan, digest string) *Reconciled {
	rec := &Reconciled{
		Identical: true,
		Probes:    map[string]rawdata.ProbePlan{},
		BProbes:   map[string]rawdata.ProbePlan{},
		DigestA:   digest,
		DigestB:   digest,
	}
	for id, pp := range plan.Probes {
		rec.Probes[id] = pp
		rec.BProbes[id] = pp
		rec.ProbeIDs = append(rec.ProbeIDs, id)
	}
	sort.Strings(rec.ProbeIDs)
	return rec
}

// parsePlanMirror 把 collection_plan 原始 JSON 解析为 compare 侧镜像结构。
func parsePlanMirror(raw json.RawMessage) (*rawdata.CollectionPlan, error) {
	var cp rawdata.CollectionPlan
	if err := json.Unmarshal(raw, &cp); err != nil {
		return nil, err
	}
	return &cp, nil
}

// planLevelDiffs 对两侧 collection_plan 去掉 probes 与 question_set_digest 后的
// 部分做泛化 diff。任一差异都是 strict 冲突：题库版本、填充与输出契约不同，
// 观测根本不可比。question_set_digest 是派生字段，单独处理（见 ReconcilePlans）。
// 解析失败时返回一条整体差异（走拒绝路径，安全侧）。
func planLevelDiffs(rawA, rawB json.RawMessage) []FieldDiff {
	strip := func(raw json.RawMessage) any {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil
		}
		delete(m, "probes")
		delete(m, "question_set_digest")
		return m
	}
	a, b := strip(rawA), strip(rawB)
	if a == nil || b == nil {
		return []FieldDiff{{Path: "(collection_plan)", A: "(unparsable)", B: "(unparsable)"}}
	}
	return DiffValues(a, b)
}

// truncDigestNote 展示用截断 digest（NOTE 里不需要全长）。
func truncDigestNote(d string) string {
	const head = len("sha256:") + 8
	if len(d) > head {
		return d[:head] + "…"
	}
	return d
}

// strictProbeDiffs 比对单个探针块的 strict 字段，返回差异（路径与
// diffPlans 一致，形如 probes.<id>.<field>，供 report.Reject 分组渲染）。
// strict 字段清单见文件头注释与 DATAFLOW §3.2。
func strictProbeDiffs(id string, pa, pb *rawdata.ProbePlan) []FieldDiff {
	path := func(field string) string { return "probes." + id + "." + field }
	var diffs []FieldDiff
	add := func(field, a, b string) {
		diffs = append(diffs, FieldDiff{Path: path(field), A: a, B: b})
	}
	if pa.ProbeVersion != pb.ProbeVersion {
		add("probe_version", pa.ProbeVersion, pb.ProbeVersion)
	}
	if pa.ObservationSchema != pb.ObservationSchema {
		add("observation_schema", pa.ObservationSchema, pb.ObservationSchema)
	}
	if pa.NormalizeRule != pb.NormalizeRule {
		add("normalize_rule", pa.NormalizeRule, pb.NormalizeRule)
	}
	if !stringsEqual(pa.CellKey, pb.CellKey) {
		add("cell_key", display(pa.CellKey), display(pb.CellKey))
	}
	if !stringsEqual(pa.QuestionIDs, pb.QuestionIDs) {
		add("question_ids", display(pa.QuestionIDs), display(pb.QuestionIDs))
	}
	if !floatPtrEqual(pa.Temperature, pb.Temperature) {
		add("temperature", displayPtr(pa.Temperature), displayPtr(pb.Temperature))
	}
	if !floatPtrEqual(pa.TopP, pb.TopP) {
		add("top_p", displayPtr(pa.TopP), displayPtr(pb.TopP))
	}
	if !intPtrEqual(pa.MaxTokens, pb.MaxTokens) {
		add("max_tokens", displayPtr(pa.MaxTokens), displayPtr(pb.MaxTokens))
	}
	if !strPtrEqual(pa.ThinkingEffort, pb.ThinkingEffort) {
		add("thinking_effort", displayPtr(pa.ThinkingEffort), displayPtr(pb.ThinkingEffort))
	}
	return diffs
}

// displayPtr 渲染指针字段：nil 显示 null（与 canonical JSON 的显式 null 一致 ——
// 「一侧没配 vs 一侧配了」是真实的请求差异，必须算冲突）。
func displayPtr[T any](v *T) string {
	if v == nil {
		return "null"
	}
	return display(jsonAny(*v))
}

// jsonAny 把标量转为 display 能识别的泛型 JSON 类型。
func jsonAny(v any) any {
	switch tv := v.(type) {
	case float64:
		return tv
	case int:
		return float64(tv)
	case string:
		return tv
	}
	return v
}

func floatPtrEqual(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func intPtrEqual(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func strPtrEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func stringsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// intsEqual 判断两个档位列表是否一致。config.Validate 保证各自严格递增，
// 因此逐位比较即可，无需排序。
func intsEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// intersectInts 求两个升序整数列表的交集，结果保持升序。
func intersectInts(a, b []int) []int {
	var out []int
	for _, x := range a {
		if intInSlice(x, b) {
			out = append(out, x)
		}
	}
	sort.Ints(out)
	return out
}

func intInSlice(v int, xs []int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func sortedProbeIDs(probes map[string]rawdata.ProbePlan) []string {
	ids := make([]string, 0, len(probes))
	for id := range probes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
