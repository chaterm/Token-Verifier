package compare

import (
	"testing"
)

// evidence_test.go：Result 上的传输直方图（描述性证据，不参与判定）。
// 每个指标一张联合直方图：A/B 共享分箱边界，counts 逐 bin 对齐。

// mkLatencyRecords 生成 n 条 onetoken record，latency/ttft 按 fn(i) 取值。
func mkLatencyRecords(n int, fn func(i int) float64) []map[string]any {
	var out []map[string]any
	for i := 0; i < n; i++ {
		out = append(out, otRecord("q1", 0, "7", map[string]any{
			"latency_ms": fn(i), "ttft_ms": fn(i) / 2, "tps": 40.0,
		}))
	}
	return out
}

func TestRunTransportHistograms(t *testing.T) {
	plan := planWith(map[string][]string{"onetoken": {"question_id", "context_bucket"}})
	// A 侧快（100~200ms），B 侧慢（800~900ms）→ 直方图应完全分离
	a := rawFile{digest: "sha256:aa", plan: plan,
		records: mkLatencyRecords(20, func(i int) float64 { return 100 + float64(i%10)*10 })}.build(t)
	b := rawFile{digest: "sha256:aa", plan: plan,
		records: mkLatencyRecords(20, func(i int) float64 { return 800 + float64(i%10)*10 })}.build(t)

	res, err := Run(a, b, map[string]float64{"onetoken": 0.15}, nil)
	if err != nil {
		t.Fatal(err)
	}

	h := res.Hists.LatencyMs
	if h.Empty() {
		t.Fatal("latency 直方图为空")
	}
	if len(h.Edges) != len(h.A)+1 || len(h.A) != len(h.B) {
		t.Fatalf("edges/counts 长度不齐: %+v", h)
	}
	if sum(h.A) != 20 || sum(h.B) != 20 {
		t.Errorf("counts 总和 = %d/%d, want 20/20", sum(h.A), sum(h.B))
	}
	// 共享边界覆盖两侧合并范围 [100, 890]
	if h.Edges[0] != 100 || h.Edges[len(h.Edges)-1] != 890 {
		t.Errorf("共享边界 = [%v, %v], want [100, 890]", h.Edges[0], h.Edges[len(h.Edges)-1])
	}
	// A 侧样本全在低端 bins、B 侧全在高端：无重叠
	for i := range h.A {
		if h.A[i] > 0 && h.B[i] > 0 {
			t.Errorf("bin %d 两侧样本重叠，与分离的数据矛盾", i)
		}
	}
	// ttft / tps 也应有直方图（tps 两侧同值 → 单 bin 退化情形）
	if res.Hists.TtftMs.Empty() || res.Hists.Tps.Empty() {
		t.Errorf("ttft/tps 直方图不应为空: %+v", res.Hists)
	}
}

func TestRunTransportHistogramsNoTtft(t *testing.T) {
	plan := planWith(map[string][]string{"onetoken": {"question_id", "context_bucket"}})
	// 无 ttft_ms 字段（非流式）→ ttft 直方图为空，latency 正常
	mk := func(v float64) map[string]any {
		return otRecord("q1", 0, "7", map[string]any{"latency_ms": v})
	}
	a := rawFile{digest: "sha256:aa", plan: plan,
		records: []map[string]any{mk(100), mk(120)}}.build(t)
	b := rawFile{digest: "sha256:aa", plan: plan,
		records: []map[string]any{mk(500), mk(520)}}.build(t)

	res, err := Run(a, b, map[string]float64{"onetoken": 0.15}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Hists.TtftMs.Empty() {
		t.Errorf("无 ttft 样本应得空直方图: %+v", res.Hists.TtftMs)
	}
	if res.Hists.LatencyMs.Empty() {
		t.Error("latency 直方图不应为空")
	}
}

func TestRunTransportHistogramsEmptyRecords(t *testing.T) {
	plan := planWith(map[string][]string{"onetoken": {"question_id", "context_bucket"}})
	a := rawFile{digest: "sha256:aa", plan: plan}.build(t)
	b := rawFile{digest: "sha256:aa", plan: plan}.build(t)

	res, err := Run(a, b, map[string]float64{"onetoken": 0.15}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 无 record → 直方图为空而不是崩溃
	if !res.Hists.LatencyMs.Empty() {
		t.Errorf("空数据应得空直方图: %+v", res.Hists.LatencyMs)
	}
}

func TestTransportHistsConsistentWithPercentiles(t *testing.T) {
	// 直方图与分位数来自同一批样本：中位数应落在累计计数过半的 bin 内
	plan := planWith(map[string][]string{"onetoken": {"question_id", "context_bucket"}})
	a := rawFile{digest: "sha256:aa", plan: plan,
		records: mkLatencyRecords(20, func(i int) float64 { return 100 + float64(i)*10 })}.build(t)
	b := rawFile{digest: "sha256:aa", plan: plan,
		records: mkLatencyRecords(20, func(i int) float64 { return 100 + float64(i)*10 })}.build(t)

	res, err := Run(a, b, map[string]float64{"onetoken": 0.15}, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := res.Hists.LatencyMs
	p50 := *res.TransportA.LatencyMs.P50
	cum := 0
	for i, c := range h.A {
		cum += c
		if cum*2 >= sum(h.A) {
			// 首个累计过半的 bin 应包含 p50
			if p50 < h.Edges[i] || p50 > h.Edges[i+1] {
				t.Errorf("p50=%v 不在累计过半的 bin [%v,%v] 内", p50, h.Edges[i], h.Edges[i+1])
			}
			break
		}
	}
}

func sum(xs []int) int {
	s := 0
	for _, x := range xs {
		s += x
	}
	return s
}
