// observe：采集期的观测提取（Observe 前移是硬性要求 ——
// digest 档 rawData 不保留原文，所有比较统计必须仅凭 observation 可算）。
// 每个探针一个函数：统一 Response → 落盘的 observation JSON。
package collect

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/chaterm/token-verifier/internal/adapter"
	"github.com/chaterm/token-verifier/internal/suite"
)

// ObserveOnetoken 提取 onetoken 观测：答案文本按题目规则归一化 → {"value": "..."}。
// jsonField 非空（输出契约生效）时先做 JSON 解包（三级容错，失败回退原文，
// 见 jsonanswer.go），再归一化。归一化后为空说明响应里没有可统计的内容
// （如全空白），按错误处理。
// 未来带 normalize 的新探针的 Observe 同样先调 extractJSONAnswer 解包。
func ObserveOnetoken(resp adapter.Response, norm *suite.Normalizer, normalizeRule, jsonField string) (json.RawMessage, error) {
	content := resp.Content
	if jsonField != "" {
		content, _ = extractJSONAnswer(content, jsonField)
	}
	value := norm.Apply(normalizeRule, content)
	if value == "" {
		return nil, fmt.Errorf("归一化后答案为空（响应无内容或不含可提取值）")
	}
	b, err := json.Marshal(map[string]string{"value": value})
	if err != nil {
		return nil, err
	}
	return b, nil
}

// ObserveTokenizer 提取 tokenizer 观测：服务端上报的输入 token 数。
// prompt_tokens 为 0 说明响应缺 usage 或端点没填，无法作为指纹，按错误处理。
func ObserveTokenizer(resp adapter.Response) (json.RawMessage, error) {
	if resp.Usage.Prompt <= 0 {
		return nil, fmt.Errorf("响应缺少有效的输入 token 数（usage.prompt=%d）", resp.Usage.Prompt)
	}
	b, err := json.Marshal(map[string]int{"prompt_tokens": resp.Usage.Prompt})
	if err != nil {
		return nil, err
	}
	return b, nil
}

// ObserveNeedle 提取 needle 观测：逐埋点标记判大小写不敏感子串包含，
// → {"matched": [true, false, ...]}。matched 是按位置序号对齐的布尔数组
// 而非单值 —— 一个埋点就是一个统计单元（SPEC-RAWDATA §7 的布尔语义按埋点
// 细化），细粒度观测保留定位信息（哪个位置漏了），比较侧按埋点粒度
// 聚合命中数做卡方。markers 为该题按位置序号升序的派生标记。
func ObserveNeedle(resp adapter.Response, markers []string) (json.RawMessage, error) {
	if len(markers) == 0 {
		return nil, fmt.Errorf("题目未派生埋点标记（needle_positions 为空）")
	}
	lower := strings.ToLower(resp.Content)
	matched := make([]bool, len(markers))
	for i, m := range markers {
		matched[i] = strings.Contains(lower, strings.ToLower(m))
	}
	b, err := json.Marshal(map[string]any{"matched": matched})
	if err != nil {
		return nil, err
	}
	return b, nil
}

// ObserveThinkEffort 提取 think-effort 观测 → {"reasoning_value": n, "unit": u}。
// 单位由协议静态决定（adapter.ReasoningUnit），不做运行时回退 ——
// 同一 cell 内两侧单位必须一致，否则秩检验失效：
//   - "tokens"：取 usage 上报的思考 token 数。缺失/为零记观测错误：
//     Go 零值无法区分「端点没上报」与「思考数为 0」，让不支持上报的
//     端点污染分布会被误判为零思考。
//   - "chars"：取交付的思考文本 rune 数（anthropic 系：usage 无独立思考
//     字段，但 thinking 块/思考流完整交付）。**0 是有效观测**：思考文本
//     就是观测通道本身，「没交付思考」与「思考为零」对该探针都是降级
//     信号 —— 基准侧交付了思考文本而 B 侧没有时，分布检验能直接判出，
//     记观测错误反而会把降级藏成 inconclusive。
func ObserveThinkEffort(resp adapter.Response, unit string) (json.RawMessage, error) {
	var value int
	if unit == "chars" {
		value = utf8.RuneCountInString(resp.ReasoningContent)
	} else {
		if resp.Usage.Reasoning <= 0 {
			return nil, fmt.Errorf("响应未上报思考 token 数（usage.reasoning=%d）", resp.Usage.Reasoning)
		}
		value = resp.Usage.Reasoning
	}
	b, err := json.Marshal(map[string]any{"reasoning_value": value, "unit": unit})
	if err != nil {
		return nil, err
	}
	return b, nil
}

// ObserveToolCall 提取 toolcall 观测：首个工具名 + 参数合法性 + 是否并行
// → {"tool": "...", "args_valid": bool, "parallel": bool, "num_calls": n}。
// args_valid：工具名在生效工具集内且参数是合法 JSON object。
// 无工具调用（模型直接回答文本）记观测错误 —— 那是工具调用能力缺失，
// 不是分布里的一个取值，按错误剔除后由可用率指标另行反映。
func ObserveToolCall(resp adapter.Response, toolNames []string) (json.RawMessage, error) {
	if len(resp.ToolCalls) == 0 {
		return nil, fmt.Errorf("响应没有工具调用（模型未发起 tool call）")
	}
	first := resp.ToolCalls[0]
	argsValid := false
	known := false
	for _, n := range toolNames {
		if n == first.Name {
			known = true
			break
		}
	}
	if known && json.Valid(first.Args) {
		var m map[string]any
		if err := json.Unmarshal(first.Args, &m); err == nil {
			argsValid = true
		}
	}
	obs := map[string]any{
		"tool":       first.Name,
		"args_valid": argsValid,
		"parallel":   len(resp.ToolCalls) > 1,
		"num_calls":  len(resp.ToolCalls),
	}
	b, err := json.Marshal(obs)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// observeFor 按探针 id 分派到对应 Observe 函数。jsonField 非空时先做契约解包；
// needleMarkers / toolNames / reasoningUnit 为对应探针的任务级参数（其余探针忽略）。
// 未实现探针的请求不会进入任务队列（collect 层已过滤），这里是兜底。
func observeFor(probeID string, resp adapter.Response, norm *suite.Normalizer, normalizeRule, jsonField string,
	needleMarkers, toolNames []string, reasoningUnit string) (json.RawMessage, error) {
	switch probeID {
	case "onetoken":
		return ObserveOnetoken(resp, norm, normalizeRule, jsonField)
	case "tokenizer":
		return ObserveTokenizer(resp)
	case "needle":
		return ObserveNeedle(resp, needleMarkers)
	case "think-effort":
		return ObserveThinkEffort(resp, reasoningUnit)
	case "toolcall":
		return ObserveToolCall(resp, toolNames)
	default:
		return nil, fmt.Errorf("探针 %q 的 Observe 未实现", probeID)
	}
}
