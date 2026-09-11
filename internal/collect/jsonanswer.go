// jsonanswer：输出契约（system prompt 强约束）的解析端。
// 契约要求模型只回答 {"<field>": "<your answer>"}；模型未必守约，
// 这里做三级容错提取，全部失败回退原文（交给既有归一化兜底，不判错）。
package collect

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// jsonFenceRe 匹配 markdown 围栏（```json ... ``` 或 ``` ... ```）。
var jsonFenceRe = regexp.MustCompile("(?s)```(?:json)?\\s*(.*?)\\s*```")

// extractJSONAnswer 从 LLM 原始回答中提取指定字段的值。
// 依次尝试：剥 markdown 围栏 → 整段 JSON 解析 → 截取首 { 到末 } 片段。
// 值兼容字符串与数字（统一转字符串）。全部失败返回 (raw, false)。
func extractJSONAnswer(raw, jsonField string) (string, bool) {
	if jsonField == "" {
		return raw, false
	}
	if m := jsonFenceRe.FindStringSubmatch(raw); len(m) == 2 {
		if v, ok := parseJSONAnswer(m[1], jsonField); ok {
			return v, true
		}
	}
	if v, ok := parseJSONAnswer(raw, jsonField); ok {
		return v, true
	}
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start >= 0 && end > start {
		if v, ok := parseJSONAnswer(raw[start:end+1], jsonField); ok {
			return v, true
		}
	}
	return raw, false
}

// parseJSONAnswer 解析 JSON 对象并提取指定字段；值可能是字符串或数字，
// 统一转为字符串。
func parseJSONAnswer(s, jsonField string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &obj); err != nil {
		return "", false
	}
	rawVal, ok := obj[jsonField]
	if !ok {
		return "", false
	}
	var strVal string
	if err := json.Unmarshal(rawVal, &strVal); err == nil {
		return strVal, true
	}
	var numVal json.Number
	if err := json.Unmarshal(rawVal, &numVal); err == nil {
		// json.Number 接受非数字字面量吗？Unmarshal 到 Number 只接受数字，
		// 浮点统一走最短往返格式。
		if f, ferr := strconv.ParseFloat(numVal.String(), 64); ferr == nil && f != float64(int64(f)) {
			return strconv.FormatFloat(f, 'f', -1, 64), true
		}
		return numVal.String(), true
	}
	return "", false
}
