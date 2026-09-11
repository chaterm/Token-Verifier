// Package plan 负责采集计划（CollectionPlan）的构建与 digest 计算。
// canonical JSON 与 digest 规则见 docs/SPEC-RAWDATA.md §3。
package plan

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/chaterm/token-verifier/internal/config"
	"github.com/chaterm/token-verifier/internal/probe"
	"github.com/chaterm/token-verifier/internal/suite"
)

// CollectionPlan 采集计划。与 rawdata.CollectionPlan 同源，
// 这里是构建侧的完整形态（compare 侧只用其中一部分字段）。
//
// digest 只锁「实验设计」（题库、探针采样参数、填充），不锁「网关接线」：
// target 段（capabilities / thinking / weight / path / temperature_field /
// body_overrides）整体不进 digest —— 同一套实验在不同网关上的拼写差异
// 不该被闸门拒绝。接线差异的效果（档位裁剪、流式折算）体现在 manifest 的
// sampling / skipped / capabilities 段，比较时对不上只出 NOTE 不拒绝。
// 每题「用哪个思考档位」由题目级 thinking_effort 承载、随题库进 digest；
// effort_map 只是该档位在具体网关上的翻译。
type CollectionPlan struct {
	SuiteVersion       string `json:"suite_version"`
	QuestionSetDigest  string `json:"question_set_digest"`
	PaddingAlgoVersion string `json:"padding_algo_version"`
	// PaddingSeed 生效的填充种子（配置值或 suite.DefaultPaddingSeed）。
	// 同 bucket 的填充文本由 seed 决定 → 必须进 digest。
	PaddingSeed int64 `json:"padding_seed"`
	// Normalize 题库提供的归一化空间（词表/扩展表）。归一化语义是
	// observation_schema 的一部分：词表不同 → 同一原文得到不同观测值，
	// 必须进 digest，否则改词表不换 suite_version 会静默错配。
	Normalize NormalizePlan `json:"normalize"`
	// OutputContract 输出契约（system prompt 强约束，SPEC-SUITE）。契约文本
	// 不同 → 对模型的格式约束不同 → 答案分布不可比，必须进 digest；
	// 纯 raw 题库为 null。
	OutputContract *OutputContractPlan  `json:"output_contract"`
	Probes         map[string]ProbePlan `json:"probes"`
}

// OutputContractPlan 输出契约的 digest 形态。
type OutputContractPlan struct {
	Field        string `json:"field"`
	SystemPrompt string `json:"system_prompt"`
}

// NormalizePlan 归一化空间的 digest 形态。空 map 统一为 {}（canonical 规则）。
type NormalizePlan struct {
	Maps    map[string]map[string][]string `json:"maps"`
	Digits  map[string]int                 `json:"digits"`
	Letters map[string]string              `json:"letters"`
}

// ProbePlan 单探针的采集计划块。
type ProbePlan struct {
	ProbeVersion      string   `json:"probe_version"`
	Repeats           int      `json:"repeats"`
	Temperature       *float64 `json:"temperature"`
	TopP              *float64 `json:"top_p"`
	MaxTokens         *int     `json:"max_tokens"`
	ContextBuckets    []int    `json:"context_buckets"`
	ThinkingEffort    *string  `json:"thinking_effort"`
	NormalizeRule     string   `json:"normalize_rule"`
	ObservationSchema string   `json:"observation_schema"`
	CellKey           []string `json:"cell_key"`
	QuestionIDs       []string `json:"question_ids"`
	// MinN 生效的单侧样本下限（配置值或 config.DefaultMinN）。
	// 影响比较侧统计判定 → 必须进 digest。
	MinN int `json:"min_n"`
}

// probeSpecs 之后是探针计划块。协议的 capabilities / thinking 与 request
// 注入配置不进 digest（见 CollectionPlan 注释）。

// Build 从配置与题库构建采集计划。只纳入启用且已实现的探针；
// 启用但未实现的探针由调用方记 skipped。
// 探针元信息（version/cell_key/observation_schema）取自 probe.Get(id).Meta()
// 单一来源 —— 观测语义变更只需递增 probe 侧，plan 自动跟随。
func Build(cfg *config.File, st *suite.File) (*CollectionPlan, error) {
	cp := &CollectionPlan{
		SuiteVersion:       st.SuiteVersion,
		PaddingAlgoVersion: suite.PaddingAlgoVersion,
		PaddingSeed:        suite.DefaultPaddingSeed,
		Normalize:          normalizePlanOf(st),
		Probes:             map[string]ProbePlan{},
	}
	if cfg.Padding.Seed != nil {
		cp.PaddingSeed = *cfg.Padding.Seed
	}
	// 契约进 digest 时取生效值（Field 空 → 默认 "answer"）
	if st.OutputContract != nil {
		cp.OutputContract = &OutputContractPlan{
			Field:        st.OutputContract.FieldOf(),
			SystemPrompt: st.OutputContract.SystemPrompt,
		}
	}

	var allQIDs []string
	for _, id := range cfg.EnabledProbes() {
		pr, implemented := probe.Get(id)
		if !implemented {
			continue // 未实现探针不进计划；调用方负责记 skipped
		}
		meta := pr.Meta()
		pc := cfg.Probes[id]
		ps, _ := st.ProbeSetOf(id)
		qids := make([]string, 0, len(ps.Items))
		for _, it := range ps.Items {
			qids = append(qids, it.ID)
		}
		sort.Strings(qids)
		allQIDs = append(allQIDs, qids...)

		norm := ps.Normalize
		cp.Probes[id] = ProbePlan{
			ProbeVersion:      meta.Version,
			Repeats:           pc.Repeats,
			Temperature:       pc.Temperature,
			TopP:              pc.TopP,
			MaxTokens:         pc.MaxTokens,
			ContextBuckets:    append([]int(nil), pc.ContextBuckets...),
			ThinkingEffort:    pc.ThinkingEffort,
			NormalizeRule:     norm,
			ObservationSchema: meta.ObservationSchema,
			CellKey:           meta.CellKey,
			QuestionIDs:       qids,
			MinN:              pc.MinNOf(id, meta.MinN),
		}
	}
	sort.Strings(allQIDs)
	cp.QuestionSetDigest = "sha256:" + hexDigest([]byte(strings.Join(allQIDs, "\n")))
	return cp, nil
}

// normalizePlanOf 把题库归一化空间转成 digest 形态（空段统一为空 map）。
func normalizePlanOf(st *suite.File) NormalizePlan {
	np := NormalizePlan{
		Maps:    st.Normalize.Maps,
		Digits:  st.Normalize.Digits,
		Letters: st.Normalize.Letters,
	}
	if np.Maps == nil {
		np.Maps = map[string]map[string][]string{}
	}
	if np.Digits == nil {
		np.Digits = map[string]int{}
	}
	if np.Letters == nil {
		np.Letters = map[string]string{}
	}
	return np
}

// Digest 计算 plan_digest：sha256 的 canonical JSON。
func (cp *CollectionPlan) Digest() (string, []byte, error) {
	canonical, err := CanonicalJSON(cp)
	if err != nil {
		return "", nil, err
	}
	return "sha256:" + hexDigest(canonical), canonical, nil
}

// ProbeDigest 计算单探针 digest：只覆盖该探针的计划块。
// 协议的 thinking 块不再折入 —— 它是网关接线细节，不进可比性契约。
func (cp *CollectionPlan) ProbeDigest(probeID string) (string, error) {
	pp, ok := cp.Probes[probeID]
	if !ok {
		return "", fmt.Errorf("plan 中不存在探针 %q", probeID)
	}
	canonical, err := CanonicalJSON(pp)
	if err != nil {
		return "", err
	}
	return "sha256:" + hexDigest(canonical), nil
}

// hexDigest 算 sha256 的十六进制。
func hexDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// CanonicalJSON 把任意可 JSON 序列化的值编码为 canonical JSON：
// 键字节序升序、无空白、数字规范化（整数不带小数点、浮点最短往返不指数）、
// 显式 null 保留、数组不排序。先用 encoding/json 转 RawMessage 树再重排，
// 这样结构体里的指针 null 也能保留。
func CanonicalJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var tree any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // 保留数字字面量，避免 float64 精度损失
	if err := dec.Decode(&tree); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := writeCanonical(&buf, tree); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeCanonical 递归写出 canonical 形态。
func writeCanonical(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case json.Number:
		buf.WriteString(normalizeNumber(t.String()))
	case string:
		// json.Marshal 对字符串的转义即最短合法形式
		b, _ := json.Marshal(t)
		buf.Write(b)
	case []any:
		buf.WriteByte('[')
		for i, x := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, x); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys) // 字节序升序
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			kb, _ := json.Marshal(k)
			buf.Write(kb)
			buf.WriteByte(':')
			if err := writeCanonical(buf, t[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("canonical JSON: 不支持的类型 %T", v)
	}
	return nil
}

// normalizeNumber 数字规范化：整数去掉小数点；浮点最短往返、不用指数。
func normalizeNumber(s string) string {
	// 解析为 float64 再判断是否为整数值；json.Number 保留了原始字面量，
	// 但 yaml 反序列化再 marshal 的数字可能是 "1" 或 "1.0" 等形态。
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return s
	}
	if f == float64(int64(f)) && f >= -9e15 && f <= 9e15 {
		return strconv.FormatInt(int64(f), 10)
	}
	// 最短往返、禁用指数
	out := strconv.FormatFloat(f, 'f', -1, 64)
	return out
}
