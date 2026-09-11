// Package probe 定义探针插件契约，并实现五个探针：
// onetoken / tokenizer / needle / think-effort / toolcall。
// 探针只做统计判定，不读文件、不发请求；采集侧的观测提取在 collect 包的 observeFor。
package probe

import (
	"encoding/json"
	"math"
)

// Observation 单条 record 的观测值原始 JSON，结构由各探针自行定义。
type Observation = json.RawMessage

// CellPair 一个 cell 的两侧观测集合（均只含 status=success 的 record）。
type CellPair struct {
	Key map[string]string // cell_key 各分量 → 值
	A   []Observation
	B   []Observation
}

// StatKind 统计量类型，决定 ratio 的算法与报告展示。
type StatKind string

const (
	StatDistance StatKind = "distance" // 越大越差（如 JSD），ratio = statistic/threshold
	StatPValue   StatKind = "pvalue"   // 越小越差（如卡方 p 值），ratio = threshold/p
)

// ProbeMeta 探针元信息。Version / CellKey / ObservationSchema 是
// 采集计划（plan）的元信息来源：plan 经 Get(id).Meta() 取值，
// 保证两侧版本号不可能脱钩。
type ProbeMeta struct {
	ID      string
	Version string   // 观测语义变更必须递增，与 ObservationSchema 的版本一致
	CellKey []string // 默认分组键；实际以 collection_plan 里的 cell_key 为准
	MinN    int      // 单侧有效样本下限的缺省值；实际以采集计划的 min_n 为准
	// ObservationSchema 观测结构标识（如 "onetoken/v2"），进 digest。
	ObservationSchema string
	StatKind          StatKind
}

// Verdict 单探针判定结果。
type Verdict struct {
	ProbeID         string
	Verdict         string   // pass | fail | inconclusive
	Statistic       *float64 // nil = 无法计算（inconclusive）
	Threshold       float64  // 用户提供
	ThresholdSource string   // config | flag，由上层填充
	Ratio           *float64 // 越限倍数，>1 恒等价 fail；为聚合层预留
	Note            string   // inconclusive 时的原因说明
	Warning         string   // 判定有效但需要知晓的边界（如检验功效不足），进报告
	Cells           []CellDetail
}

// CellDetail 单 cell 明细，进 JSON 报告。
type CellDetail struct {
	Key          map[string]string
	NA           int
	NB           int
	Stat         *float64       // 逐 cell 统计量（onetoken: JSD；tokenizer: 见 Extra.match）
	Insufficient bool           // 样本量不足，已从统计中剔除
	Extra        map[string]any // 探针特有字段
}

// Probe 探针契约：只含比较侧；采集侧的观测提取（Observe）在 collect 包。
// minN 为该探针的生效样本下限（来自采集计划，进 digest）；
// minN < 1 时实现方回退 Meta().MinN —— 兼容没有 min_n 字段的旧 rawData。
type Probe interface {
	Meta() ProbeMeta
	Compare(pairs []CellPair, threshold float64, minN int) Verdict
}

// registry 已实现探针的注册表。
var registry = map[string]Probe{
	"onetoken":     onetokenProbe{},
	"tokenizer":    tokenizerProbe{},
	"needle":       needleProbe{},
	"think-effort": thinkEffortProbe{},
	"toolcall":     toolcallProbe{},
}

// Get 按 ID 查探针；未实现的返回 false。
func Get(id string) (Probe, bool) {
	p, ok := registry[id]
	return p, ok
}

// ratio 计算越限倍数。distance 类为 statistic/threshold，
// p 值类为 threshold/p，两者都满足 >1 ⟺ fail。
// +Inf（p 恰为 0）用最大浮点数顶替，因为 JSON 无法表示无穷。
func ratio(kind StatKind, statistic, threshold float64) float64 {
	var r float64
	switch kind {
	case StatDistance:
		r = statistic / threshold
	case StatPValue:
		if statistic <= 0 {
			r = math.Inf(1)
		} else {
			r = threshold / statistic
		}
	}
	if math.IsInf(r, 1) {
		r = math.MaxFloat64
	}
	return r
}
