// Package compare 实现比较阶段：digest 闸门、cell 配对、逐探针判定与汇总。
package compare

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/chaterm/token-verifier/internal/rawdata"
)

// FieldDiff 一处字段级差异，路径为点分隔（如 probes.onetoken.repeats）。
type FieldDiff struct {
	Path string
	A    string // 展示用字符串
	B    string
}

// GateError 采集计划不兼容，比较被拒绝（exit 3）。
type GateError struct {
	DigestA, DigestB string
	Diffs            []FieldDiff
	FormatVersionA   int
	FormatVersionB   int
}

func (e *GateError) Error() string {
	return fmt.Sprintf("incompatible collection plans: %s vs %s", e.DigestA, e.DigestB)
}

// CheckGate 执行 digest 闸门：plan_digest 与 format_version 任一不等即拒绝。
// 拒绝时对两侧 collection_plan 做泛化逐字段 diff，帮助定位冲突（DATAFLOW §3.2）。
func CheckGate(a, b *rawdata.File) error {
	if a.Manifest.FormatVersion != b.Manifest.FormatVersion {
		return &GateError{
			DigestA:        a.Manifest.PlanDigest,
			DigestB:        b.Manifest.PlanDigest,
			FormatVersionA: a.Manifest.FormatVersion,
			FormatVersionB: b.Manifest.FormatVersion,
		}
	}
	if a.Manifest.PlanDigest == b.Manifest.PlanDigest {
		return nil
	}
	// digest 不等：解析两侧计划做字段级 diff。解析失败不阻断 —— diff 只是给人看的定位信息
	diffs := diffPlans(a.Manifest.CollectionPlan, b.Manifest.CollectionPlan)
	return &GateError{
		DigestA:        a.Manifest.PlanDigest,
		DigestB:        b.Manifest.PlanDigest,
		FormatVersionA: a.Manifest.FormatVersion,
		FormatVersionB: b.Manifest.FormatVersion,
		Diffs:          diffs,
	}
}

// diffPlans 把两侧 collection_plan 解析成泛型结构后递归比对。
func diffPlans(rawA, rawB json.RawMessage) []FieldDiff {
	var a, b any
	if err := json.Unmarshal(rawA, &a); err != nil {
		return nil
	}
	if err := json.Unmarshal(rawB, &b); err != nil {
		return nil
	}
	return DiffValues(a, b)
}

// DiffValues 对两个已解析的泛型 JSON 值做递归字段级 diff，结果按路径排序。
func DiffValues(a, b any) []FieldDiff {
	var diffs []FieldDiff
	diffValue("", a, b, &diffs)
	sort.Slice(diffs, func(i, j int) bool { return diffs[i].Path < diffs[j].Path })
	return diffs
}

// DiffDigests 供 run 的采集前预检：把基线 manifest 的 collection_plan 与
// 本次将要采集的计划做字段级 diff（计划本身还没落盘，只有 digest 和结构体）。
func DiffDigests(baselinePlan json.RawMessage, cp any) []FieldDiff {
	var a any
	if err := json.Unmarshal(baselinePlan, &a); err != nil {
		return nil
	}
	// cp 是 *plan.CollectionPlan；为避免 compare 依赖 plan 包，
	// 这里经 JSON 往返转成泛型值再 diff
	raw, err := json.Marshal(cp)
	if err != nil {
		return nil
	}
	var b any
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil
	}
	return DiffValues(a, b)
}

// diffValue 递归比较两个 JSON 值，差异追加到 diffs。
func diffValue(path string, a, b any, diffs *[]FieldDiff) {
	if sameJSON(a, b) {
		return
	}
	am, aIsObj := a.(map[string]any)
	bm, bIsObj := b.(map[string]any)
	if aIsObj && bIsObj {
		// 对象：逐键比对，含单侧缺失（present yes/no）
		keys := make(map[string]bool, len(am)+len(bm))
		for k := range am {
			keys[k] = true
		}
		for k := range bm {
			keys[k] = true
		}
		for k := range keys {
			av, inA := am[k]
			bv, inB := bm[k]
			child := k
			if path != "" {
				child = path + "." + k
			}
			switch {
			case !inA:
				*diffs = append(*diffs, FieldDiff{Path: child, A: absent, B: display(bv)})
			case !inB:
				*diffs = append(*diffs, FieldDiff{Path: child, A: display(av), B: absent})
			default:
				diffValue(child, av, bv, diffs)
			}
		}
		return
	}
	*diffs = append(*diffs, FieldDiff{Path: path, A: display(a), B: display(b)})
}

const absent = "(absent)"

// sameJSON 判断两个泛型 JSON 值是否相等。
// 数字统一按 float64 比（encoding/json 解出的 number 都是 float64），
// 数组逐元素比，对象逐键比。
func sameJSON(a, b any) bool {
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for k, v := range av {
			bvv, has := bv[k]
			if !has || !sameJSON(v, bvv) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !sameJSON(av[i], bv[i]) {
				return false
			}
		}
		return true
	case float64:
		bv, ok := b.(float64)
		return ok && av == bv
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case nil:
		return b == nil
	}
	return false
}

// display 把泛型 JSON 值渲染为紧凑的展示字符串。
func display(v any) string {
	switch tv := v.(type) {
	case nil:
		return "null"
	case string:
		return tv
	case float64:
		// 整数不带小数点，与 canonical JSON 规则一致
		if tv == float64(int64(tv)) {
			return fmt.Sprintf("%d", int64(tv))
		}
		return fmt.Sprintf("%g", tv)
	case bool:
		return fmt.Sprintf("%t", tv)
	}
	// 数组与对象：紧凑 JSON
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}
