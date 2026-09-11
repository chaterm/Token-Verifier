// collect 包编排采集：构建计划 → 比例分配 → 能力裁剪 → 洗牌 → 执行 → 落盘。
// 本文件是比例分配（最大余数法）与能力裁剪。
package collect

import (
	"fmt"
	"sort"

	"github.com/chaterm/token-verifier/internal/config"
	"github.com/chaterm/token-verifier/internal/rawdata"
)

// Allocation 分配结果：每个组合分到的请求数。
type Allocation struct {
	Counts  map[string]int // 键 "protocol\x00transport"
	Skipped []rawdata.Skip
}

// CountOf 某组合分到的数量。
func (a Allocation) CountOf(protocol, transport string) int {
	return a.Counts[protocol+"\x00"+transport]
}

// Keys 返回所有数量 > 0 的组合，按 (协议, 传输) 字典序（确定性）。
func (a Allocation) Keys() [][2]string {
	var out [][2]string
	for k, n := range a.Counts {
		if n <= 0 {
			continue
		}
		parts := splitKey(k)
		out = append(out, [2]string{parts[0], parts[1]})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i][0] != out[j][0] {
			return out[i][0] < out[j][0]
		}
		return out[i][1] < out[j][1]
	})
	return out
}

func splitKey(k string) [2]string {
	for i := 0; i < len(k); i++ {
		if k[i] == 0 {
			return [2]string{k[:i], k[i+1:]}
		}
	}
	return [2]string{k, ""}
}

// LargestRemainder 最大余数法（SPEC-CONFIG §5.2）：
// Σalloc 恰等于 N；小数部分相同时按数组下标升序，保证确定性。
func LargestRemainder(n int, weights []float64) []int {
	out := make([]int, len(weights))
	if n <= 0 || len(weights) == 0 {
		return out
	}
	var total float64
	for _, w := range weights {
		total += w
	}
	if total <= 0 {
		return out
	}
	exact := make([]float64, len(weights))
	var baseSum int
	for i, w := range weights {
		exact[i] = float64(n) * w / total
		out[i] = int(exact[i])
		baseSum += out[i]
	}
	rem := n - baseSum
	// 按 (exact - base) 降序，平局按下标升序
	idx := make([]int, len(weights))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		fa := exact[idx[a]] - float64(out[idx[a]])
		fb := exact[idx[b]] - float64(out[idx[b]])
		if fa != fb {
			return fa > fb
		}
		return idx[a] < idx[b]
	})
	for i := 0; i < rem && i < len(idx); i++ {
		out[idx[i]]++
	}
	return out
}

// AllocateProbe 为一个探针的一个 cell 分配 repeats：
// 先按协议 weight 切，再按各协议的 transport 比例切。
// 能力裁剪：stream:false 砍掉流式份额（进 skipped）；
// context_window < bucket 砍掉整个 cell（进 skipped）；
// needsTools 且 tools:false / needsThinking 且 thinking:false 砍掉整个协议份额。
func AllocateProbe(probeID string, bucket int, repeats int, protocols []config.ProtocolConfig, needsTools, needsThinking bool) Allocation {
	alloc := Allocation{Counts: map[string]int{}}

	// 协议级权重（裁掉超窗与能力不满足的协议）
	var weights []float64
	var avail []config.ProtocolConfig
	for _, p := range protocols {
		if p.Weight <= 0 {
			continue
		}
		if bucket > 0 && p.Capabilities.ContextWindow > 0 && bucket > p.Capabilities.ContextWindow {
			alloc.Skipped = append(alloc.Skipped, rawdata.Skip{
				ProbeID:  probeID,
				Protocol: p.ID,
				Reason:   "context_window",
				Detail:   fmt.Sprintf("bucket %d 超过声明窗口 %d，该档位不生成请求", bucket, p.Capabilities.ContextWindow),
			})
			continue
		}
		if needsTools && !p.Capabilities.Tools {
			alloc.Skipped = append(alloc.Skipped, rawdata.Skip{
				ProbeID:  probeID,
				Protocol: p.ID,
				Reason:   "tools_unsupported",
				Detail:   "协议声明 tools: false，不生成 toolcall 请求",
			})
			continue
		}
		if needsThinking && !p.Capabilities.Thinking {
			alloc.Skipped = append(alloc.Skipped, rawdata.Skip{
				ProbeID:  probeID,
				Protocol: p.ID,
				Reason:   "thinking_unsupported",
				Detail:   "协议声明 thinking: false，不生成 think-effort 请求",
			})
			continue
		}
		avail = append(avail, p)
		weights = append(weights, p.Weight)
	}
	if len(avail) == 0 {
		return alloc
	}

	protoCounts := LargestRemainder(repeats, weights)
	for i, p := range avail {
		n := protoCounts[i]
		if n <= 0 {
			continue
		}
		// 协议内按 transport 比例切；stream:false 的份额折到 non_stream
		streamW, nonStreamW := p.Transport.Stream, p.Transport.NonStream
		if !p.Capabilities.Stream {
			if streamW > 0 {
				alloc.Skipped = append(alloc.Skipped, rawdata.Skip{
					ProbeID:  probeID,
					Protocol: p.ID,
					Reason:   "stream_unsupported",
					Detail:   "协议声明 stream: false，流式份额折算到非流式",
				})
			}
			streamW = 0
		}
		counts := LargestRemainder(n, []float64{nonStreamW, streamW})
		if counts[1] > 0 {
			alloc.Counts[p.ID+"\x00"+"stream"] += counts[1]
		}
		if counts[0] > 0 {
			alloc.Counts[p.ID+"\x00"+"non_stream"] += counts[0]
		}
	}
	return alloc
}
