// Package config 负责配置文件加载、校验与阈值解析。
// 配置文件格式见 docs/SPEC-CONFIG.md。
package config

import (
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// File 配置文件顶层结构。
type File struct {
	Version    int                    `yaml:"version"`
	Target     Target                 `yaml:"target"`
	Suite      SuiteRef               `yaml:"suite"`
	Probes     map[string]ProbeConfig `yaml:"probes"`
	Thresholds map[string]float64     `yaml:"thresholds"`
	Runtime    Runtime                `yaml:"runtime"`
	Padding    Padding                `yaml:"padding"`
}

// Padding 长上下文填充配置。seed 影响实际送入端点的文本 → 必须进 digest。
type Padding struct {
	// Seed 填充文本生成种子；nil = 取 suite.DefaultPaddingSeed（旧行为）。
	Seed *int64 `yaml:"seed"`
}

// Target 待测端点。compare 不需要，collect/run 必填。
type Target struct {
	BaseURL   string           `yaml:"base_url"`
	APIKeyEnv string           `yaml:"api_key_env"` // 只读环境变量名，密钥不写入配置
	Model     string           `yaml:"model"`
	Protocols []ProtocolConfig `yaml:"protocols"`
	Request   RequestConfig    `yaml:"request"`
}

// ProtocolConfig 单个协议的配置。
type ProtocolConfig struct {
	ID           string        `yaml:"id"`
	Weight       float64       `yaml:"weight"`
	Path         string        `yaml:"path"` // 可选，覆盖协议默认路径
	Capabilities Capabilities  `yaml:"capabilities"`
	Transport    TransportMix  `yaml:"transport"`
	Thinking     ThinkingBlock `yaml:"thinking"`
}

// Capabilities 声明式能力。工具信任声明，不做探测。
// 不进 digest（网关接线细节）；原样记录在 manifest 供归因。
// 缺省 {true,true,true,0}（SPEC-CONFIG §3.2）。
// Present 记录 YAML 里是否显式给出该段 —— 用来区分「没写」与
// 「显式写了全 false」（纯文本端点是合法配置，不能被缺省值翻转）。
type Capabilities struct {
	Stream        bool `yaml:"stream" json:"stream"`
	Tools         bool `yaml:"tools" json:"tools"`
	Thinking      bool `yaml:"thinking" json:"thinking"`
	ContextWindow int  `yaml:"context_window" json:"context_window"` // 0 = 不按窗口裁档位
	Present       bool `yaml:"-" json:"-"`
}

// UnmarshalYAML 区分字段缺失与显式 false：缺失的字段取缺省值 true，
// 显式给出的尊重原值。整段缺失时本方法不会被调用，由 ApplyDefaults 兜底。
func (c *Capabilities) UnmarshalYAML(value *yaml.Node) error {
	var raw struct {
		Stream        *bool `yaml:"stream"`
		Tools         *bool `yaml:"tools"`
		Thinking      *bool `yaml:"thinking"`
		ContextWindow *int  `yaml:"context_window"`
	}
	if err := value.Decode(&raw); err != nil {
		return err
	}
	c.Present = true
	c.Stream = raw.Stream == nil || *raw.Stream
	c.Tools = raw.Tools == nil || *raw.Tools
	c.Thinking = raw.Thinking == nil || *raw.Thinking
	if raw.ContextWindow != nil {
		c.ContextWindow = *raw.ContextWindow
	}
	return nil
}

// TransportMix 协议内流式/非流式的相对比例，归一化后切分该协议的份额。
type TransportMix struct {
	Stream    float64 `yaml:"stream"`
	NonStream float64 `yaml:"non_stream"`
}

// ThinkingBlock 思考配置：disabled（题目无强度时用）与 enabled（有强度时用）。
// effort_map 的值类型因协议而异（字符串级别或整数预算），用 any 保留。
type ThinkingBlock struct {
	Disabled ThinkingMode `yaml:"disabled"`
	Enabled  ThinkingMode `yaml:"enabled"`
}

// ThinkingMode 一个思考状态下的字段注入。
type ThinkingMode struct {
	EffortMap     map[string]any `yaml:"effort_map"`
	BodyOverrides map[string]any `yaml:"body_overrides"`
}

// RequestConfig 所有协议共享的请求字段。
type RequestConfig struct {
	TemperatureField string         `yaml:"temperature_field"` // 点路径，默认 temperature
	BodyOverrides    map[string]any `yaml:"body_overrides"`
}

// SuiteRef 题库引用。suite 一律外部文件提供，工具不内置。
type SuiteRef struct {
	Path   string `yaml:"path"`
	SHA256 string `yaml:"sha256"` // 可选；给了必须校验通过
}

// ProbeConfig 单探针的采集参数（进 digest）。
type ProbeConfig struct {
	Enabled        bool     `yaml:"enabled"`
	Repeats        int      `yaml:"repeats"` // 每 cell 总请求数，按比例切分
	Temperature    *float64 `yaml:"temperature"`
	TopP           *float64 `yaml:"top_p"`
	MaxTokens      *int     `yaml:"max_tokens"`
	ContextBuckets []int    `yaml:"context_buckets"`
	ThinkingEffort *string  `yaml:"thinking_effort"` // 探针级默认；题目级优先
	// MinN 单侧有效样本下限（比较侧判定 insufficient 的门槛）。nil = 取 DefaultMinN。
	// 影响统计判定 → 必须进 digest。
	MinN *int `yaml:"min_n"`
}

// Runtime 运行时参数，全部不进 digest。
type Runtime struct {
	MaxConcurrency int    `yaml:"max_concurrency"`
	TimeoutSec     int    `yaml:"timeout_sec"`
	MinIntervalMs  int    `yaml:"min_interval_ms"`
	MaxAttempts    int    `yaml:"max_attempts"` // 含首次
	Seed           int64  `yaml:"seed"`
	RawLevel       string `yaml:"raw_level"` // digest | full
	// 重试退避：第 attempt 次重试前等待 min(base << attempt, max) 毫秒。
	BackoffBaseMs int `yaml:"backoff_base_ms"`
	BackoffMaxMs  int `yaml:"backoff_max_ms"`
}

// Load 读取 YAML 配置文件，并应用缺省值。
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}
	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("配置文件解析失败: %w", err)
	}
	f.ApplyDefaults()
	return &f, nil
}

// ApplyDefaults 填缺省值。capabilities 缺省 {true,true,true,0}：
// 省略与显式写全 true 等价（Present 标记区分「没写」与「显式全 false」）。
func (f *File) ApplyDefaults() {
	if f.Runtime.MaxConcurrency <= 0 {
		f.Runtime.MaxConcurrency = 4
	}
	if f.Runtime.TimeoutSec <= 0 {
		f.Runtime.TimeoutSec = 120
	}
	if f.Runtime.MaxAttempts <= 0 {
		f.Runtime.MaxAttempts = 3
	}
	if f.Runtime.RawLevel == "" {
		f.Runtime.RawLevel = "digest"
	}
	if f.Runtime.BackoffBaseMs <= 0 {
		f.Runtime.BackoffBaseMs = 500
	}
	if f.Runtime.BackoffMaxMs <= 0 {
		f.Runtime.BackoffMaxMs = 30000
	}
	if f.Target.Request.TemperatureField == "" {
		f.Target.Request.TemperatureField = "temperature"
	}
	for i := range f.Target.Protocols {
		// capabilities 整段缺省（未写）时给缺省值 {true,true,true,0}。
		// 写了的段由 UnmarshalYAML 按字段处理，显式全 false 不会被翻转。
		if !f.Target.Protocols[i].Capabilities.Present {
			f.Target.Protocols[i].Capabilities = Capabilities{Stream: true, Tools: true, Thinking: true, Present: true}
		}
	}
}

// knownProtocols 已知协议标识集合。
var knownProtocols = map[string]bool{
	"openai-chat":        true,
	"openai-responses":   true,
	"anthropic-messages": true,
}

// Validate 落实 SPEC-CONFIG §10 的校验规则（结构性 1-8、探针与题库 9-14、
// 注入与冲突 15-17）。任一失败返回错误并指明字段路径；警告经 warn 回调不阻断。
// suiteItems 为每个启用探针的题库题目（含题目级 thinking_effort），由调用方加载。
func (f *File) Validate(suiteItems map[string][]SuiteItemInfo, warn func(string)) error {
	// —— 结构性 ——
	if f.Version != 1 {
		return fmt.Errorf("version: 必须为 1，收到 %d", f.Version)
	}
	if strings.TrimSpace(f.Target.BaseURL) == "" {
		return fmt.Errorf("target.base_url: 不能为空")
	}
	if !strings.HasPrefix(f.Target.BaseURL, "http://") && !strings.HasPrefix(f.Target.BaseURL, "https://") {
		return fmt.Errorf("target.base_url: 必须是合法 URL（http/https），收到 %q", f.Target.BaseURL)
	}
	if strings.TrimSpace(f.Target.APIKeyEnv) == "" {
		return fmt.Errorf("target.api_key_env: 不能为空（只接受环境变量名，不接受密钥本身）")
	}
	if os.Getenv(f.Target.APIKeyEnv) == "" {
		return fmt.Errorf("target.api_key_env: 环境变量 %s 未设置", f.Target.APIKeyEnv)
	}
	if strings.TrimSpace(f.Target.Model) == "" {
		return fmt.Errorf("target.model: 不能为空")
	}
	if len(f.Target.Protocols) == 0 {
		return fmt.Errorf("target.protocols: 至少一项")
	}
	seen := map[string]bool{}
	positive := 0
	for i, p := range f.Target.Protocols {
		if !knownProtocols[p.ID] {
			return fmt.Errorf("target.protocols[%d].id: 未知协议 %q", i, p.ID)
		}
		if seen[p.ID] {
			return fmt.Errorf("target.protocols[%d].id: 协议 %q 重复", i, p.ID)
		}
		seen[p.ID] = true
		if p.Weight > 0 {
			positive++
		}
		if p.Transport.Stream <= 0 && p.Transport.NonStream <= 0 {
			return fmt.Errorf("target.protocols[%d].transport: stream 与 non_stream 至少一项 > 0", i)
		}
	}
	if positive == 0 {
		return fmt.Errorf("target.protocols: 至少一个协议 weight > 0")
	}
	if strings.TrimSpace(f.Suite.Path) == "" {
		return fmt.Errorf("suite.path: 必填")
	}
	if _, err := os.Stat(f.Suite.Path); err != nil {
		return fmt.Errorf("suite.path: %v", err)
	}
	switch f.Runtime.RawLevel {
	case "digest":
	case "full":
		// full 档要求落盘原文与 SSE chunk，当前版本未实现。
		// 静默降级成 digest 会让用户以为文件里有原文，明确拒绝。
		return fmt.Errorf("runtime.raw_level: full 档尚未实现（原文与 SSE chunk 落盘），请使用 digest 或降低版本预期")
	default:
		return fmt.Errorf("runtime.raw_level: 未知取值 %q（digest | full）", f.Runtime.RawLevel)
	}

	// —— 探针与题库 ——
	enabled := f.EnabledProbes()
	if len(enabled) == 0 {
		return fmt.Errorf("probes: 至少一个探针 enabled: true")
	}
	for _, id := range enabled {
		p := f.Probes[id]
		if p.Repeats < 1 {
			return fmt.Errorf("probes.%s.repeats: 必须 ≥ 1", id)
		}
		if len(p.ContextBuckets) == 0 {
			return fmt.Errorf("probes.%s.context_buckets: 不能为空", id)
		}
		if p.MinN != nil && *p.MinN < 1 {
			return fmt.Errorf("probes.%s.min_n: 必须 ≥ 1", id)
		}
		for i, b := range p.ContextBuckets {
			if b < 0 {
				return fmt.Errorf("probes.%s.context_buckets[%d]: 必须为非负整数", id, i)
			}
			if i > 0 && b <= p.ContextBuckets[i-1] {
				return fmt.Errorf("probes.%s.context_buckets: 必须严格递增", id)
			}
		}
		items := suiteItems[id]
		if len(items) == 0 {
			return fmt.Errorf("probes.%s: 题库中不存在该探针的题目", id)
		}
		// 题目用到的每个 effort 级别，所有 weight>0 协议的 effort_map 都得有映射
		usedEfforts := map[string]bool{}
		for _, it := range items {
			eff := it.ThinkingEffort
			if eff == "" {
				eff = strVal(p.ThinkingEffort)
			}
			if eff != "" {
				usedEfforts[eff] = true
			}
		}
		for eff := range usedEfforts {
			for _, proto := range f.Target.Protocols {
				if proto.Weight <= 0 {
					continue
				}
				if _, ok := proto.Thinking.Enabled.EffortMap[eff]; !ok {
					return fmt.Errorf("probes.%s: 题目用到 thinking_effort=%q，但协议 %s 的 enabled.effort_map 没有该级别的映射", id, eff, proto.ID)
				}
			}
		}
		// 警告：不阻断
		if id == "onetoken" && p.Temperature != nil && *p.Temperature < 0.5 {
			warn(fmt.Sprintf("probes.onetoken.temperature=%v < 0.5：分布可能退化，探针有效性下降", *p.Temperature))
		}
		if min, ok := DefaultMinN[id]; ok {
			effMinN := p.MinNOf(id, min)
			if p.Repeats < effMinN {
				warn(fmt.Sprintf("probes.%s.repeats=%d 低于生效 min_n %d：可能大量 cell 被判 insufficient", id, p.Repeats, effMinN))
			}
			// think-effort 的 cell 按协议分层：每协议的期望样本 =
			// repeats × 权重份额，任一份额不足 min_n 该协议的 cell 全部
			// insufficient（探针仍可从其它协议出结论，但样本被切碎）
			if id == "think-effort" {
				totalW := 0.0
				for _, proto := range f.Target.Protocols {
					if proto.Weight > 0 {
						totalW += proto.Weight
					}
				}
				if totalW > 0 {
					for _, proto := range f.Target.Protocols {
						if proto.Weight <= 0 {
							continue
						}
						expect := float64(p.Repeats) * proto.Weight / totalW
						if expect < float64(effMinN) {
							warn(fmt.Sprintf("probes.think-effort: 协议 %s 的期望样本 %.1f（repeats=%d × 权重份额）低于 min_n %d，该协议的 cell 将全部判 insufficient；提高 repeats 或权重",
								proto.ID, expect, p.Repeats, effMinN))
						}
					}
				}
			}
		}
	}

	// —— 阈值键（静态检查；阈值缺失/数值合法性在 ResolveThresholds 判定时查）——
	// 档位键 <probe>.<bucket> 的后缀必须是整数，且档位在该探针的
	// context_buckets 内 —— 笔误（如 onetoken.8001）静默不生效是最坑的失败模式。
	// 只查启用探针：未启用/未知探针的键宽容忽略（与判定侧行为一致）。
	enabledSet := map[string]bool{}
	for _, id := range enabled {
		enabledSet[id] = true
	}
	for key := range f.Thresholds {
		probeID, suffix, has := strings.Cut(key, ".")
		if !has || !enabledSet[probeID] {
			continue
		}
		b, err := strconv.Atoi(suffix)
		if err != nil {
			return fmt.Errorf("thresholds.%s: 档位后缀应为整数（形如 <probe>.<bucket>，例 onetoken.8000）", key)
		}
		if !intIn(b, f.Probes[probeID].ContextBuckets) {
			return fmt.Errorf("thresholds.%s: 档位 %d 不在 probes.%s.context_buckets %v 中",
				key, b, probeID, f.Probes[probeID].ContextBuckets)
		}
	}

	// —— 注入与冲突 ——
	tempPath := f.Target.Request.TemperatureField
	for loc, ov := range map[string]map[string]any{"target.request.body_overrides": f.Target.Request.BodyOverrides} {
		if err := checkReserved(ov, loc); err != nil {
			return err
		}
	}
	for i, p := range f.Target.Protocols {
		for _, blk := range []struct {
			name string
			mode ThinkingMode
		}{
			{"disabled", p.Thinking.Disabled},
			{"enabled", p.Thinking.Enabled},
		} {
			loc := fmt.Sprintf("target.protocols[%d].thinking.%s.body_overrides", i, blk.name)
			if err := checkReserved(blk.mode.BodyOverrides, loc); err != nil {
				return err
			}
			// ${thinking_effort} 只许出现在存在 effort_map 的 enabled 块
			if blk.name == "disabled" || len(blk.mode.EffortMap) == 0 {
				if containsEffortPlaceholder(blk.mode.BodyOverrides) {
					return fmt.Errorf("%s: ${thinking_effort} 只能出现在存在 effort_map 的 enabled 块中", loc)
				}
			}
		}
		// enabled.body_overrides 与 temperature_field 指向同一路径 → 冲突
		if pathExists(p.Thinking.Enabled.BodyOverrides, tempPath) {
			return fmt.Errorf("target.protocols[%d] (%s): thinking.enabled.body_overrides 与 request.temperature_field 指向同一路径 %q", i, p.ID, tempPath)
		}
	}
	return nil
}

// SuiteItemInfo 校验所需的题目最小信息。
type SuiteItemInfo struct {
	ID             string
	ThinkingEffort string // 题目级覆盖；空 = 未设置
}

// EnabledProbes 返回启用的探针 ID（排序，保证确定性）。
func (f *File) EnabledProbes() []string {
	out := make([]string, 0, len(f.Probes))
	for id, p := range f.Probes {
		if p.Enabled {
			out = append(out, id)
		}
	}
	sortStrings(out)
	return out
}

// DefaultMinN 各探针的缺省最小样本量（min_n 未配置时的生效值）。
// 这是唯一默认值来源：比较侧（probe）与采集计划（plan）都引用它，
// 校验警告的 repeats 建议值同样由此派生 —— 避免同一个 10 双写在多处。
// needle=1：埋点命中的统计单元是埋点（repeats 内全中判定），单元自身
// 不要求单侧样本下限；think-effort/toolcall=5：分布/比率类统计的小样本底线。
var DefaultMinN = map[string]int{
	"onetoken":     10,
	"tokenizer":    1,
	"needle":       1,
	"think-effort": 5,
	"toolcall":     5,
}

// MinNOf 返回探针的生效 min_n：配置了取配置值，否则取 DefaultMinN，
// 再否则取 fallback（比较侧传入探针的 Meta().MinN）。
func (p ProbeConfig) MinNOf(id string, fallback int) int {
	if p.MinN != nil {
		return *p.MinN
	}
	if v, ok := DefaultMinN[id]; ok {
		return v
	}
	return fallback
}

// ReservedFields 不允许经 body_overrides 设置的字段（单一来源，adapter 渲染时复用）。
// system 在列：输出契约的 system prompt 由工具按题库注入（anthropic 形态是顶层字段）。
// tools 在列：toolcall 探针的工具集由题库掌控，各协议的包装形态由适配器渲染。
var ReservedFields = map[string]bool{"model": true, "messages": true, "input": true, "system": true, "tools": true}

// checkReserved 检查 body_overrides 不含保留字段。
func checkReserved(ov map[string]any, loc string) error {
	for k := range ov {
		if ReservedFields[k] {
			return fmt.Errorf("%s: 不允许设置保留字段 %q（model 与消息体字段由工具掌控）", loc, k)
		}
	}
	return nil
}

const effortPlaceholder = "${thinking_effort}"

// containsEffortPlaceholder 递归查找占位符。
func containsEffortPlaceholder(v any) bool {
	switch t := v.(type) {
	case string:
		return strings.Contains(t, effortPlaceholder)
	case map[string]any:
		for _, x := range t {
			if containsEffortPlaceholder(x) {
				return true
			}
		}
	case []any:
		for _, x := range t {
			if containsEffortPlaceholder(x) {
				return true
			}
		}
	}
	return false
}

// pathExists 判断点路径（如 generation_config.temperature）是否已在 overrides 里存在。
func pathExists(ov map[string]any, dotPath string) bool {
	cur := ov
	for _, seg := range strings.Split(dotPath, ".") {
		next, ok := cur[seg]
		if !ok {
			return false
		}
		m, ok := next.(map[string]any)
		if !ok {
			return true // 末段已存在（任意类型都算占用该路径）
		}
		cur = m
	}
	return true
}

func strVal(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// ResolvedThreshold 一个探针（或探针的某个上下文档位）的已解析阈值及其来源。
type ResolvedThreshold struct {
	Value  float64
	Source string // "flag" | "config"
}

// ProbeThresholds 一个探针的阈值集合：档位（context_bucket）专用值 + 探针级兜底。
// 配置与 --threshold 均支持 <probe>.<bucket> 形式的档位键（如 onetoken.8000）。
type ProbeThresholds struct {
	Default  *ResolvedThreshold // probe 级兜底；各档位已全覆盖时可为 nil
	ByBucket map[int]ResolvedThreshold
}

// For 返回某档位生效的阈值：档位专用 > 探针级兜底。
func (t ProbeThresholds) For(bucket int) (ResolvedThreshold, bool) {
	if th, ok := t.ByBucket[bucket]; ok {
		return th, true
	}
	if t.Default != nil {
		return *t.Default, true
	}
	return ResolvedThreshold{}, false
}

// ResolveThresholds 为每个待比较探针解析阈值，支持档位级覆盖。
// probes 为探针 ID → 其 context_buckets（来自采集计划），用于：
//  1. 枚举需要校验阈值的档位；
//  2. 拒绝档位键里不在 context_buckets 内的笔误（如 onetoken.8001）。
//
// 同一档位内优先级：flag <probe>.<bucket> > config <probe>.<bucket>
// > flag <probe> > config <probe>；未知探针的键宽容忽略（与旧版一致）。
// 任一探针的任一档位缺阈值即报错（错误信息指名缺哪个）；阈值必须为有限正数，
// p 值类（alpha）还须在 (0,1) 内 —— 由调用方按探针统计量类型传入 pValueProbes。
func ResolveThresholds(probes map[string][]int, flagVals map[string]float64, cfg *File, pValueProbes map[string]bool) (map[string]ProbeThresholds, error) {
	cfgVals := map[string]float64{}
	if cfg != nil {
		cfgVals = cfg.Thresholds
	}
	// 档位键先按「键 → (probe, bucket, 来源)」拆解并校验，再逐探针解析。
	type bucketKey struct {
		probeID string
		bucket  int
	}
	bucketKeys := map[bucketKey]bool{}
	for _, m := range []map[string]float64{flagVals, cfgVals} {
		for key := range m {
			probeID, suffix, has := strings.Cut(key, ".")
			if !has {
				continue
			}
			b, err := strconv.Atoi(suffix)
			if err != nil {
				return nil, fmt.Errorf("阈值键非法: %q，档位后缀应为整数（形如 <probe>.<bucket>，例 onetoken.8000）", key)
			}
			bucketKeys[bucketKey{probeID, b}] = true
		}
	}
	// 档位必须在该探针的 context_buckets 内，否则静默不生效 —— 笔误要报错。
	for _, id := range sortedKeys(probes) {
		for bk := range bucketKeys {
			if bk.probeID != id {
				continue
			}
			if !intIn(bk.bucket, probes[id]) {
				return nil, fmt.Errorf("阈值键非法: %s.%d，档位 %d 不在 probes.%s.context_buckets %v 中",
					id, bk.bucket, bk.bucket, id, probes[id])
			}
		}
	}

	validate := func(key string, val float64) error {
		// NaN 与任何数比较都为 false，会绕过下面的区间检查并让判定恒 pass；
		// ±Inf 同样让某一侧判定失效。三者都必须在入口拒绝。
		if math.IsNaN(val) || math.IsInf(val, 0) {
			return fmt.Errorf("阈值非法: %s = %v，必须是有限正数", key, val)
		}
		if val <= 0 {
			return fmt.Errorf("阈值非法: %s = %v，必须为正数", key, val)
		}
		// 档位键（onetoken.8000）按探针 ID 归到 p 值类
		probeID, _, _ := strings.Cut(key, ".")
		if pValueProbes[probeID] && val >= 1 {
			return fmt.Errorf("阈值非法: %s = %v，alpha 须在 (0,1) 内", key, val)
		}
		return nil
	}

	out := make(map[string]ProbeThresholds, len(probes))
	for _, id := range sortedKeys(probes) {
		var pt ProbeThresholds
		// 探针级兜底：flag > config
		if v, has := flagVals[id]; has {
			if err := validate(id, v); err != nil {
				return nil, err
			}
			pt.Default = &ResolvedThreshold{Value: v, Source: "flag"}
		} else if v, has := cfgVals[id]; has {
			if err := validate(id, v); err != nil {
				return nil, err
			}
			pt.Default = &ResolvedThreshold{Value: v, Source: "config"}
		}
		// 档位级：flag <probe>.<bucket> > config <probe>.<bucket>
		for _, b := range probes[id] {
			key := fmt.Sprintf("%s.%d", id, b)
			var th ResolvedThreshold
			if v, has := flagVals[key]; has {
				th = ResolvedThreshold{Value: v, Source: "flag"}
			} else if v, has := cfgVals[key]; has {
				th = ResolvedThreshold{Value: v, Source: "config"}
			} else {
				continue // 无档位专用值，交给 Default 兜底
			}
			if err := validate(key, th.Value); err != nil {
				return nil, err
			}
			if pt.ByBucket == nil {
				pt.ByBucket = map[int]ResolvedThreshold{}
			}
			pt.ByBucket[b] = th
		}
		// 兜底缺失时：每个待比较档位都必须有专用值
		if pt.Default == nil {
			for _, b := range probes[id] {
				if _, has := pt.ByBucket[b]; !has {
					return nil, MissingThresholdError{ProbeID: id, Bucket: &b}
				}
			}
			// 无档位（旧 rawData / 自定义计划）也没有兜底 → 缺阈值
			if len(probes[id]) == 0 {
				return nil, MissingThresholdError{ProbeID: id}
			}
		}
		out[id] = pt
	}
	return out, nil
}

// sortedKeys 返回 map 键的排序切片，保证确定性。
func sortedKeys(m map[string][]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// intIn 判断 v 是否在列表内。
func intIn(v int, list []int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// MissingThresholdError 缺少某探针（或探针的某档位）的阈值。
// 工具不提供默认阈值（SPEC-CONFIG §7）。
type MissingThresholdError struct {
	ProbeID string
	Bucket  *int // 非 nil = 缺该档位的专用阈值且无探针级兜底
}

func (e MissingThresholdError) Error() string {
	if e.Bucket != nil {
		return fmt.Sprintf("no threshold for probe %s (context_bucket=%d)\n"+
			"  用 --threshold %s.%d=<value> 或 --threshold %s=<value>（兜底）提供，\n"+
			"  或在配置文件的 thresholds 段中设置。本工具不提供默认阈值：\n"+
			"  合适的阈值取决于模型自身的随机性、部署环境与你的容忍度，只能由使用者标定。",
			e.ProbeID, *e.Bucket, e.ProbeID, *e.Bucket, e.ProbeID)
	}
	return fmt.Sprintf("no threshold for probe %s\n"+
		"  用 --threshold %s=<value> 提供，或在配置文件的 thresholds 段中设置。\n"+
		"  本工具不提供默认阈值：合适的阈值取决于模型自身的随机性、部署环境\n"+
		"  与你的容忍度，只能由使用者标定。", e.ProbeID, e.ProbeID)
}

// ParseThresholdFlag 解析 --threshold <probe>=<value> 形式的参数。
func ParseThresholdFlag(s string) (string, float64, error) {
	id, valStr, found := strings.Cut(s, "=")
	if !found || id == "" {
		return "", 0, fmt.Errorf("--threshold 格式应为 <probe>=<value>，收到 %q", s)
	}
	val, err := strconv.ParseFloat(valStr, 64)
	if err != nil {
		return "", 0, fmt.Errorf("--threshold %s: %q 不是合法数字", id, valStr)
	}
	return id, val, nil
}
