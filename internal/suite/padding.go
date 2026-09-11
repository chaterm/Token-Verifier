package suite

// padding 生成：长上下文填充文本。算法版本进入采集计划的
// padding_algo_version —— 同样的 bucket 必须生成同样的文本，
// 否则同一 bucket 在不同采集中实际送进的文本不同。
//
// needle 探针在采集时把派生标记按题目声明的相对位置埋入填充文本
// （AssembleNeedleContext），其余探针的 context_buckets 通常为 [0]，
// padding 不会被实际触发。

// PaddingAlgoVersion 当前填充算法版本。
// v2：seed 由代码常量改为配置提供（缺省 DefaultPaddingSeed），
// 生效 seed 经采集计划 padding_seed 进 digest —— 跨环境一致性由
// 「配置相同 → digest 相同 → 闸门保证」承接。
const PaddingAlgoVersion = "padding/v2"

// DefaultPaddingSeed 缺省填充种子（= padding/v1 时代的写死值 12345）。
// 配置未写 padding.seed 时生效，保证旧行为可复现。
const DefaultPaddingSeed int64 = 12345

// paddingWords 填充词表：中性、无实义、跨语言混合，
// 避免与任何探针题目的答案域重合。
var paddingWords = []string{
	"the", "of", "and", "in", "to", "was", "is", "for", "with", "as",
	"on", "at", "by", "an", "be", "this", "have", "from", "or", "one",
	"had", "not", "but", "what", "all", "were", "they", "we", "when",
	"your", "can", "said", "there", "each", "which", "she", "do", "how",
	"their", "if", "will", "up", "other", "about", "out", "many", "then",
}

// GeneratePadding 为给定 context_bucket 生成确定性填充文本。
// bucket 为 0 时返回空串（不加填充）。同一 (bucket, seed) 恒产出同一文本。
// 长度为近似值：按英文单词平均 ~1.3 token 估算，不要求精确。
func GeneratePadding(bucket int, seed int64) string {
	if bucket <= 0 {
		return ""
	}
	targetWords := bucket * 3 / 4 // 近似：1 token ≈ 0.75 词
	if targetWords < 1 {
		targetWords = 1
	}
	// 种子驱动的简单 LCG，保证跨运行确定
	state := uint64(bucket)*2654435761 + uint64(seed)
	out := make([]string, 0, targetWords)
	for i := 0; i < targetWords; i++ {
		state = state*6364136223846793005 + 1442695040888963407
		out = append(out, paddingWords[(state>>33)%uint64(len(paddingWords))])
	}
	return joinWords(out)
}

func joinWords(ws []string) string {
	n := 0
	for _, w := range ws {
		n += len(w) + 1
	}
	b := make([]byte, 0, n)
	for i, w := range ws {
		if i > 0 {
			b = append(b, ' ')
		}
		b = append(b, w...)
	}
	return string(b)
}
