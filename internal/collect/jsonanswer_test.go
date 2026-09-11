package collect

import "testing"

// extractJSONAnswer：输出契约（system prompt 强约束）的解析端。
// 三级容错移植自 purity-dector-kernel（question-bank/internal/generator/normalize.go）。

func TestExtractJSONAnswerPlain(t *testing.T) {
	got, ok := extractJSONAnswer(`{"answer": "42"}`, "answer")
	if !ok || got != "42" {
		t.Errorf("纯 JSON = (%q, %v), want (42, true)", got, ok)
	}
}

func TestExtractJSONAnswerNumericValue(t *testing.T) {
	// 值可能是数字而非字符串，统一转字符串
	got, ok := extractJSONAnswer(`{"answer": 42}`, "answer")
	if !ok || got != "42" {
		t.Errorf("数字值 = (%q, %v), want (42, true)", got, ok)
	}
	got2, ok2 := extractJSONAnswer(`{"answer": 3.5}`, "answer")
	if !ok2 || got2 != "3.5" {
		t.Errorf("浮点值 = (%q, %v), want (3.5, true)", got2, ok2)
	}
}

func TestExtractJSONAnswerFenced(t *testing.T) {
	raw := "```json\n{\"answer\": \"red\"}\n```"
	got, ok := extractJSONAnswer(raw, "answer")
	if !ok || got != "red" {
		t.Errorf("fence 包裹 = (%q, %v), want (red, true)", got, ok)
	}
	// 无 json 标记的裸围栏
	raw2 := "```\n{\"answer\": \"blue\"}\n```"
	if got, ok := extractJSONAnswer(raw2, "answer"); !ok || got != "blue" {
		t.Errorf("裸围栏 = (%q, %v), want (blue, true)", got, ok)
	}
}

func TestExtractJSONAnswerEmbedded(t *testing.T) {
	raw := `好的，答案是 {"answer": "7"}，希望有帮助。`
	got, ok := extractJSONAnswer(raw, "answer")
	if !ok || got != "7" {
		t.Errorf("嵌入文本 = (%q, %v), want (7, true)", got, ok)
	}
}

func TestExtractJSONAnswerFallback(t *testing.T) {
	// 解析失败：返回原文、ok=false（调用方回退到原文归一化，不判错）
	cases := []string{
		"42",                          // 无 JSON
		`{"wrong_field": "x"}`,        // 字段不存在
		"",                            // 空串
		`{"answer": `,                 // 残缺 JSON
		`{"answer": {"nested": "v"}}`, // 值是对象，非字符串/数字
	}
	for _, raw := range cases {
		got, ok := extractJSONAnswer(raw, "answer")
		if ok {
			t.Errorf("extractJSONAnswer(%q) ok=true, want false", raw)
		}
		if got != raw {
			t.Errorf("extractJSONAnswer(%q) = %q, want 原文回退", raw, got)
		}
	}
}
