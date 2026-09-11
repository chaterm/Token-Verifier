// Package suite 负责题库文件（suite）的加载、校验与答案归一化。
// 格式见 docs/SPEC-SUITE.md；suite 一律外部文件提供，工具不内置题库。
//
// 归一化空间的归属：机理规则（number/letter/word/none 的提取算法）在代码里，
// 词表类规则（如 color）的词表由题库的 normalize 段提供 —— 程序内不写死任何词表。
// 词表属于题目内容，随题库外置、可审查、可自定义，并经采集计划进入 digest。
package suite

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

// File 一份题库。
type File struct {
	SuiteVersion string `yaml:"suite_version"`
	// OutputContract 输出契约（system prompt 强约束）。凡生效 normalize ≠ none
	// 的探针自动应用（ContractFor）；纯 raw 题库可为 nil。格式见 docs/SPEC-SUITE.md。
	OutputContract *OutputContract     `yaml:"output_contract"`
	Normalize      NormalizeSpace      `yaml:"normalize"`
	Probes         map[string]ProbeSet `yaml:"probes"`
}

// OutputContract 输出契约：要求模型只回答 {"<field>": "<your answer>"} 的
// JSON 对象。文本由题库提供（可审查、可自定义），应用规则在代码里。
type OutputContract struct {
	Field        string `yaml:"field"`         // JSON 答案字段名；空 = "answer"
	SystemPrompt string `yaml:"system_prompt"` // 注入的 system prompt 文本
}

// FieldOf 返回契约的答案字段名（空时取默认 "answer"）。
func (c *OutputContract) FieldOf() string {
	if c.Field == "" {
		return "answer"
	}
	return c.Field
}

// ContractFor 输出契约的机械应用规则：生效 normalize 规则为空或 none
// （不从回复文本提取答案）→ raw，返回 nil；其余规则 → 返回题库契约。
// 新探针只要声明 normalize 即自动获得强约束，无需逐探针适配。
func (f *File) ContractFor(normalizeRule string) *OutputContract {
	if normalizeRule == "" || normalizeRule == "none" {
		return nil
	}
	return f.OutputContract
}

// NormalizeSpace 题库提供的归一化空间。
type NormalizeSpace struct {
	// Maps 词表规则：规则名 → 规范值 → 同义词列表（如 color: {red: [red, 红, rouge]}）。
	// 任何名字都可以定义（不限于 color），探针/题目的 normalize 引用它即生效。
	Maps map[string]map[string][]string `yaml:"maps"`
	// Digits 扩展 number 规则的字符→数值表（如带圈数字 ⑦: 7）。单字符键，值 0-9。
	Digits map[string]int `yaml:"digits"`
	// Letters 扩展 letter 规则的字符→规范形式表（如 é: e）。单字符键。
	Letters map[string]string `yaml:"letters"`
}

// ProbeSet 一个探针的题目集合与探针级默认。
type ProbeSet struct {
	Normalize      string  `yaml:"normalize"`
	ThinkingEffort *string `yaml:"thinking_effort"`
	// Tools 探针级工具集（仅 toolcall）；题目级 tools 覆盖它。
	Tools []Tool `yaml:"tools"`
	Items []Item `yaml:"items"`
}

// Tool 一个虚构工具的定义（SPEC-SUITE）。Parameters 是 JSON schema。
type Tool struct {
	Name        string         `yaml:"name"`
	Description string         `yaml:"description"`
	Parameters  map[string]any `yaml:"parameters"`
}

// Item 一道题。题目级字段覆盖探针级默认。
type Item struct {
	ID             string  `yaml:"id"`
	Prompt         string  `yaml:"prompt"`
	Normalize      string  `yaml:"normalize"`       // 空 = 继承探针级
	ThinkingEffort *string `yaml:"thinking_effort"` // nil = 继承探针级
	// NeedlePositions 埋点相对位置列表（仅 needle，∈(0,1)）。
	// 一个位置 = 一个埋点 = 比较侧一个统计单元；缺省 [0.5]。
	NeedlePositions []float64 `yaml:"needle_positions"`
	// Tools 题目级工具集覆盖（仅 toolcall）；nil = 继承探针级。
	Tools []Tool `yaml:"tools"`
	// ExpectParallel 标注该题期望并行调用（仅 toolcall，进报告不进判定）。
	ExpectParallel bool `yaml:"expect_parallel"`
}

// PositionsOf 解析该题实际生效的埋点位置（升序钳制；缺省 [0.5]）。
func (s ProbeSet) PositionsOf(it Item) []float64 {
	pos := it.NeedlePositions
	if len(pos) == 0 {
		pos = []float64{0.5}
	}
	return SortPositions(pos)
}

// ToolsOf 解析该题实际生效的工具集（题目级优先）。
func (s ProbeSet) ToolsOf(it Item) []Tool {
	if it.Tools != nil {
		return it.Tools
	}
	return s.Tools
}

// NormalizeOf 解析该题实际生效的归一化规则（题目级优先）。
func (s ProbeSet) NormalizeOf(it Item) string {
	if it.Normalize != "" {
		return it.Normalize
	}
	return s.Normalize
}

// ThinkingEffortOf 解析该题实际生效的思考强度（题目级优先；空串 = null）。
func (s ProbeSet) ThinkingEffortOf(it Item) string {
	if it.ThinkingEffort != nil {
		return *it.ThinkingEffort
	}
	if s.ThinkingEffort != nil {
		return *s.ThinkingEffort
	}
	return ""
}

// Load 读取 suite 文件；给了 wantSHA256 则校验完整性。
func Load(path, wantSHA256 string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取题库失败: %w", err)
	}
	if wantSHA256 != "" {
		sum := sha256.Sum256(data)
		if got := hex.EncodeToString(sum[:]); !strings.EqualFold(got, wantSHA256) {
			return nil, fmt.Errorf("题库 sha256 校验失败: 期望 %s，实际 %s", wantSHA256, got)
		}
	}
	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("题库解析失败: %w", err)
	}
	if err := f.Validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

// builtinRules 机理规则：提取算法在代码里，不需要词表。
var builtinRules = map[string]bool{
	"number": true, "letter": true, "word": true, "none": true,
}

// KnownRule 判断规则名是否可用：机理规则或题库定义了词表的规则。
func (f *File) KnownRule(rule string) bool {
	if builtinRules[rule] {
		return true
	}
	_, ok := f.Normalize.Maps[rule]
	return ok
}

// Validate 检查 suite 自身合法性（SPEC-SUITE §7）：
// id 唯一、normalize 属于机理规则或题库已定义的词表规则、扩展表键值合法。
func (f *File) Validate() error {
	if f.SuiteVersion == "" {
		return fmt.Errorf("suite_version: 不能为空")
	}
	// 归一化空间自身的合法性
	for ch, d := range f.Normalize.Digits {
		if len([]rune(ch)) != 1 {
			return fmt.Errorf("normalize.digits: 键 %q 必须是单个字符", ch)
		}
		if d < 0 || d > 9 {
			return fmt.Errorf("normalize.digits[%q]: 值必须在 0-9 之间，收到 %d", ch, d)
		}
	}
	for ch, v := range f.Normalize.Letters {
		if len([]rune(ch)) != 1 {
			return fmt.Errorf("normalize.letters: 键 %q 必须是单个字符", ch)
		}
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("normalize.letters[%q]: 规范形式不能为空", ch)
		}
	}
	for rule, vocab := range f.Normalize.Maps {
		if len(vocab) == 0 {
			return fmt.Errorf("normalize.maps.%s: 词表不能为空", rule)
		}
		for canon, syns := range vocab {
			if strings.TrimSpace(canon) == "" {
				return fmt.Errorf("normalize.maps.%s: 规范值不能为空", rule)
			}
			for _, s := range syns {
				if strings.TrimSpace(s) == "" {
					return fmt.Errorf("normalize.maps.%s.%s: 同义词不能为空", rule, canon)
				}
			}
		}
	}

	seen := map[string]bool{}
	needsContract := false
	for pid, ps := range f.Probes {
		if ps.Normalize != "" && !f.KnownRule(ps.Normalize) {
			return fmt.Errorf("probes.%s.normalize: 未知规则 %q（机理规则为 number/letter/word/none；词表规则须在题库 normalize.maps 段定义）", pid, ps.Normalize)
		}
		if err := validateTools(pid, "", ps.Tools); err != nil {
			return err
		}
		for _, it := range ps.Items {
			if it.ID == "" {
				return fmt.Errorf("probes.%s: 存在无 id 的题目", pid)
			}
			if seen[it.ID] {
				return fmt.Errorf("probes.%s: 题目 id %q 重复", pid, it.ID)
			}
			seen[it.ID] = true
			n := ps.NormalizeOf(it)
			if n != "" && !f.KnownRule(n) {
				return fmt.Errorf("probes.%s.items[%s].normalize: 未知规则 %q（机理规则为 number/letter/word/none；词表规则须在题库 normalize.maps 段定义）", pid, it.ID, n)
			}
			if n != "" && n != "none" {
				needsContract = true
			}
			for _, pos := range it.NeedlePositions {
				if pos <= 0 || pos >= 1 {
					return fmt.Errorf("probes.%s.items[%s].needle_positions: 位置 %v 必须在 (0,1) 开区间内（占语料长度的比例）", pid, it.ID, pos)
				}
			}
			if err := validateTools(pid, it.ID, it.Tools); err != nil {
				return err
			}
		}
	}
	// 有 normalize（≠ none）的题目就必须有输出契约：强约束与归一化提取
	// 是一对配套语义，缺一边观测质量无法保证（SPEC-SUITE）。
	if needsContract && (f.OutputContract == nil || strings.TrimSpace(f.OutputContract.SystemPrompt) == "") {
		return fmt.Errorf("存在 normalize ≠ none 的题目，但 output_contract 段缺失或 system_prompt 为空")
	}
	return nil
}

// validateTools 检查工具集合法性：name 非空且集合内唯一、parameters 为
// object 形态的 JSON schema。itemID 为空串表示探针级工具集。
func validateTools(pid, itemID string, tools []Tool) error {
	scope := "probes." + pid
	if itemID != "" {
		scope = fmt.Sprintf("probes.%s.items[%s]", pid, itemID)
	}
	seen := map[string]bool{}
	for _, t := range tools {
		if strings.TrimSpace(t.Name) == "" {
			return fmt.Errorf("%s.tools: 存在无 name 的工具", scope)
		}
		if seen[t.Name] {
			return fmt.Errorf("%s.tools: 工具名 %q 重复", scope, t.Name)
		}
		seen[t.Name] = true
		if len(t.Parameters) == 0 {
			return fmt.Errorf("%s.tools[%s].parameters: 不能为空（JSON schema，object 形态）", scope, t.Name)
		}
		if typ, _ := t.Parameters["type"].(string); typ != "" && typ != "object" {
			return fmt.Errorf("%s.tools[%s].parameters.type: 必须是 object，收到 %q", scope, t.Name, typ)
		}
	}
	return nil
}

// ItemsOf 返回某探针的题目列表（无该探针段时返回 nil）。
func (f *File) ItemsOf(probeID string) []Item {
	ps, ok := f.Probes[probeID]
	if !ok {
		return nil
	}
	return ps.Items
}

// ProbeSetOf 返回某探针的题目集合。
func (f *File) ProbeSetOf(probeID string) (ProbeSet, bool) {
	ps, ok := f.Probes[probeID]
	return ps, ok
}

// —— 归一化执行 ——

// Normalizer 归一化执行器：题库归一化空间 + 内置机理。
// 构造后只读，可并发使用；同一输入恒得同一输出（观测可复现的硬要求）。
type Normalizer struct {
	maps    map[string]map[string][]string
	digits  map[rune]int
	letters map[rune]string
}

// Normalizer 构造绑定该题库归一化空间的执行器。
func (f *File) Normalizer() *Normalizer {
	n := &Normalizer{
		maps:    f.Normalize.Maps,
		digits:  make(map[rune]int, len(f.Normalize.Digits)),
		letters: make(map[rune]string, len(f.Normalize.Letters)),
	}
	for ch, d := range f.Normalize.Digits {
		n.digits[[]rune(ch)[0]] = d
	}
	for ch, v := range f.Normalize.Letters {
		n.letters[[]rune(ch)[0]] = v
	}
	return n
}

// Apply 按规则把原始响应文本压成可统计的答案值。
// 未知规则原样返回（去空白）—— 加载时已校验，这里只是兜底。
func (n *Normalizer) Apply(rule, raw string) string {
	s := strings.TrimSpace(raw)
	if vocab, ok := n.maps[rule]; ok {
		return n.applyVocab(vocab, s)
	}
	switch rule {
	case "number":
		return n.normalizeNumber(s)
	case "letter":
		return n.normalizeLetter(s)
	case "word":
		return normalizeWord(s)
	default: // none 及未知规则
		return s
	}
}

// applyVocab 词表匹配：大小写不敏感子串匹配；多个命中时最长同义词胜出，
// 长度相同按规范名字典序 —— 与 map 遍历序无关，结果确定（"red-orange"
// 恒归到能匹配的最长词）。未命中保留原文。
func (n *Normalizer) applyVocab(vocab map[string][]string, s string) string {
	lower := strings.ToLower(s)
	canons := make([]string, 0, len(vocab))
	for c := range vocab {
		canons = append(canons, c)
	}
	sort.Strings(canons)

	bestCanon, bestLen := "", -1
	for _, canon := range canons {
		for _, syn := range vocab[canon] {
			l := strings.ToLower(syn)
			if len(l) > bestLen && strings.Contains(lower, l) {
				bestCanon, bestLen = canon, len(l)
			}
		}
	}
	if bestCanon == "" {
		return s
	}
	return bestCanon
}

// —— number：提取第一个数字 ——
// 支持阿拉伯/全角/阿拉伯-印度数字（含题库 digits 扩展）与中文复合数词
// （十/百/千/万/亿 位值组合，如「四十二」→ 42、「一百二十三」→ 123）。

var cnDigits = map[rune]int64{
	'零': 0, '〇': 0, '一': 1, '二': 2, '两': 2, '三': 3, '四': 4,
	'五': 5, '六': 6, '七': 7, '八': 8, '九': 9,
	'壹': 1, '贰': 2, '叁': 3, '肆': 4, '伍': 5, '陆': 6, '柒': 7, '捌': 8, '玖': 9,
}

var cnUnits = map[rune]int64{
	'十': 10, '拾': 10, '百': 100, '佰': 100, '千': 1000, '仟': 1000,
	'万': 10000, '萬': 10000, '亿': 100000000, '億': 100000000,
}

func (n *Normalizer) normalizeNumber(s string) string {
	rs := []rune(s)
	for i := 0; i < len(rs); {
		if out, _, ok := n.scanArabic(rs, i); ok {
			return out
		}
		if out, ok := n.scanCJK(rs, i); ok {
			return out
		}
		i++
	}
	return ""
}

// scanArabic 从 i 起扫一段阿拉伯系数字（可含一个小数点）。
func (n *Normalizer) scanArabic(rs []rune, i int) (string, int, bool) {
	var b strings.Builder
	dot := false
	j := i
	for ; j < len(rs); j++ {
		if d, ok := n.digitOf(rs[j]); ok {
			b.WriteByte('0' + byte(d))
			continue
		}
		if rs[j] == '.' && !dot && b.Len() > 0 && j+1 < len(rs) {
			if _, ok := n.digitOf(rs[j+1]); ok {
				dot = true
				b.WriteByte('.')
				continue
			}
		}
		break
	}
	if b.Len() == 0 {
		return "", i, false
	}
	return b.String(), j, true
}

// scanCJK 从 i 起扫一段连续的中文数词并解析位值。
func (n *Normalizer) scanCJK(rs []rune, i int) (string, bool) {
	j := i
	for ; j < len(rs); j++ {
		if _, ok := cnDigits[rs[j]]; ok {
			continue
		}
		if _, ok := cnUnits[rs[j]]; ok {
			continue
		}
		break
	}
	if j == i {
		return "", false
	}
	return formatInt(parseCN(rs[i:j])), true
}

// parseCN 解析中文数词位值：十百千为节内单位，万亿为节权。
func parseCN(rs []rune) int64 {
	var total, section, num int64
	for i, r := range rs {
		if d, ok := cnDigits[r]; ok {
			num = d
			continue
		}
		u := cnUnits[r]
		switch {
		case u < 10000: // 十/百/千
			if num == 0 && u == 10 && i == 0 {
				num = 1 // 「十五」= 15
			}
			section += num * u
			num = 0
		case u == 10000: // 万：本节乘权后并入总数
			section = (section + num) * u
			total += section
			section, num = 0, 0
		default: // 亿：全部乘权
			total = (total + section + num) * u
			section, num = 0, 0
		}
	}
	return total + section + num
}

func formatInt(v int64) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

// digitOf 题库扩展表优先，其次内置数字体系（阿拉伯/阿拉伯-印度/全角）。
func (n *Normalizer) digitOf(r rune) (int, bool) {
	if d, ok := n.digits[r]; ok {
		return d, true
	}
	switch {
	case r >= '0' && r <= '9':
		return int(r - '0'), true
	case r >= 0x0660 && r <= 0x0669:
		return int(r - 0x0660), true
	case r >= 0x06F0 && r <= 0x06F9:
		return int(r - 0x06F0), true
	case r >= 0xFF10 && r <= 0xFF19:
		return int(r - 0xFF10), true
	}
	return 0, false
}

// normalizeLetter 提取第一个字母并转小写：题库 letters 扩展表优先
// （大小写不敏感查表），其次内置的全角 ASCII 字母折叠与半角字母。
func (n *Normalizer) normalizeLetter(s string) string {
	for _, r := range s {
		if v, ok := n.letters[r]; ok {
			return v
		}
		if v, ok := n.letters[unicode.ToLower(r)]; ok {
			return v
		}
		switch {
		case r >= 'a' && r <= 'z':
			return string(r)
		case r >= 'A' && r <= 'Z':
			return string(r + ('a' - 'A'))
		case r >= 0xFF41 && r <= 0xFF5A: // 全角小写
			return string(r - 0xFF41 + 'a')
		case r >= 0xFF21 && r <= 0xFF3A: // 全角大写
			return string(r - 0xFF21 + 'a')
		}
	}
	return ""
}

// normalizeWord 取第一个词，转小写。
func normalizeWord(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	return strings.ToLower(fields[0])
}
