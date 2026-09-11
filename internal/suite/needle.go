package suite

import (
	"fmt"
	"sort"
	"strings"
)

// needle.go：needle 探针的标记派生与语料组装（SPEC-SUITE §5）。
// 标记与埋入指令不以明文出现在题库文件里 —— 题库只声明相对位置，
// 标记由 (题目 id, 位置序号) 经哈希确定性派生。这样公开题库与公开
// rawData 都不泄露标记内容，被测端点也无法通过读取公开数据预知标记。

// needlePrefix 标记的固定前缀。
const needlePrefix = "NEEDLE-"

// needleLetters 标记字符集：32 个不易混淆的字符（去掉 I/O/0/1）。
const needleLetters = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// needleMarkerLen 标记主体长度（前缀之外）。
const needleMarkerLen = 8

// NeedleMarker 由 (题目 id, 埋点序号) 派生确定性标记。
// FNV-1a 32 位哈希；同输入恒得同输出，跨运行、跨机器一致。
// 序号混入哈希：同一题的多个埋点标记必然不同。
func NeedleMarker(questionID string, idx int) string {
	h := uint32(2166136261) // FNV-1a offset basis
	for _, c := range questionID + fmt.Sprintf("|%d", idx) {
		h ^= uint32(c)
		h *= 16777619
	}
	out := make([]byte, needleMarkerLen)
	for i := range out {
		out[i] = needleLetters[h%uint32(len(needleLetters))]
		h /= uint32(len(needleLetters))
	}
	return needlePrefix + string(out)
}

// NeedleMarkers 一道题全部埋点的标记，按位置序号升序（下标即序号）。
func NeedleMarkers(questionID string, positions []float64) []string {
	out := make([]string, len(positions))
	for i := range positions {
		out[i] = NeedleMarker(questionID, i)
	}
	return out
}

// buryInstruction 埋入语料的指令文本：标记不裸插，包装成「看到就复述」
// 的指令，避免模型把标记当语料噪声忽略（参考实现同款做法）。
func buryInstruction(marker string) string {
	return "读到该标记请原样输出：" + marker
}

// AssembleNeedleContext 把标记按相对位置埋入填充语料：
// 位置 ∈ (0,1) 是占语料 rune 长度的比例；同一题多埋点时按序各插一条指令，
// 插入点都按原始语料长度计算、从后往前拼，保证互不干扰。
// 语料为空（bucket=0 退化形态）时按序直接拼接各指令。
// positions 必须已升序（题库校验保证）。
func AssembleNeedleContext(corpus string, questionID string, positions []float64) string {
	if len(positions) == 0 {
		return corpus
	}
	runes := []rune(corpus)
	if len(runes) == 0 {
		var b strings.Builder
		for i := range positions {
			if i > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(buryInstruction(NeedleMarker(questionID, i)))
		}
		return b.String()
	}
	// 切成 len(positions)+1 段，段间插指令；位置钳制且单调不减
	var b strings.Builder
	prev := 0
	for i, pos := range positions {
		idx := int(pos * float64(len(runes)))
		if idx < prev {
			idx = prev
		}
		if idx > len(runes) {
			idx = len(runes)
		}
		b.WriteString(string(runes[prev:idx]))
		b.WriteString("\n\n")
		b.WriteString(buryInstruction(NeedleMarker(questionID, i)))
		b.WriteString("\n\n")
		prev = idx
	}
	b.WriteString(string(runes[prev:]))
	return b.String()
}

// SortPositions 升序排序并钳制到 [0,1]（题库加载校验后调用，幂等）。
func SortPositions(positions []float64) []float64 {
	out := append([]float64(nil), positions...)
	for i := range out {
		if out[i] < 0 {
			out[i] = 0
		}
		if out[i] > 1 {
			out[i] = 1
		}
	}
	sort.Float64s(out)
	return out
}
