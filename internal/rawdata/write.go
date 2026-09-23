// rawData 写入侧：多成员 gzip 追加写、续跑状态读回、aggregates 构建。
// 文件格式见 docs/SPEC-RAWDATA.md；「文件即自身续跑状态」的语义：
// manifest 第 1 行、record 边采边追加、aggregates 收尾时写。
package rawdata

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/chaterm/token-verifier/internal/stats"
)

// FormatVersion 当前 rawData 格式版本。
const FormatVersion = 1

// ToolVersion 工具版本，写进 manifest 元信息（不参与 digest）。
// 发布时由 goreleaser 经 -ldflags 注入（如 v1.0.0）；源码构建时回退到 dev。
var ToolVersion = "dev"

// Writer rawData 追加写器。每批写一个独立 gzip 成员（gzip 支持多成员串联，
// 解压端天然拼接），避免「一次 gzip 流中途崩溃 → 整个文件不可读」。
type Writer struct {
	path string
	f    *os.File
	gz   *gzip.Writer
	bw   *bufio.Writer
}

// Append 打开（或创建）文件并追加一个新 gzip 成员。
// 调用方负责保证 manifest 已是第 1 行 —— 新建文件时先 WriteManifest。
func Append(path string) (*Writer, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		// 目录不存在 / 权限不足都走这里：不透 OS 原文，路径自带
		switch {
		case os.IsNotExist(err):
			return nil, &fileError{msg: "rawData 输出路径不可达", path: path, reason: "父目录不存在", err: err}
		case os.IsPermission(err):
			return nil, &fileError{msg: "rawData 文件不可写", path: path, reason: "权限不足", err: err}
		default:
			return nil, &fileError{msg: "rawData 文件不可写", path: path, err: err}
		}
	}
	w := &Writer{path: path, f: f}
	w.gz, err = gzip.NewWriterLevel(f, gzip.BestSpeed)
	if err != nil {
		f.Close()
		return nil, err
	}
	w.bw = bufio.NewWriter(w.gz)
	return w, nil
}

// WriteLine 写一行 NDJSON。
func (w *Writer) WriteLine(line []byte) error {
	if _, err := w.bw.Write(line); err != nil {
		return err
	}
	return w.bw.WriteByte('\n')
}

// Flush 把已写内容推到磁盘（gzip 局部块 + 文件 Sync）。
// 边采边落盘依赖它：Flush 之后进程崩溃，已写行仍可被读回（Reader 容忍截断尾部）。
func (w *Writer) Flush() error {
	if err := w.bw.Flush(); err != nil {
		return err
	}
	if err := w.gz.Flush(); err != nil {
		return err
	}
	return w.f.Sync()
}

// WriteRecord 序列化并写一条 record。
func (w *Writer) WriteRecord(rec Record) error {
	rec.Kind = "record"
	b, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("序列化 record 失败: %w", err)
	}
	return w.WriteLine(b)
}

// Close 结束当前 gzip 成员并关闭文件。后续追加会开启新成员。
func (w *Writer) Close() error {
	if err := w.bw.Flush(); err != nil {
		_ = w.f.Close()
		return err
	}
	if err := w.gz.Close(); err != nil {
		_ = w.f.Close()
		return err
	}
	return w.f.Close()
}

// EnsureFile 为采集准备输出文件：
//   - 不存在 → 写入 manifest（第 1 行），返回空 resume 集合
//   - 存在   → 读回校验 plan_digest 一致（否则拒绝续写）；
//     若已有 aggregates 尾行（上次收官后追加采集），重写文件去掉该行；
//     返回已完成（status=success）的 ResumeKey 集合
func EnsureFile(path string, manifest Manifest) (done map[ResumeKey]bool, err error) {
	if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
		w, err := Append(path)
		if err != nil {
			return nil, err
		}
		manifest.Kind = "manifest"
		b, err := json.Marshal(manifest)
		if err != nil {
			_ = w.Close()
			return nil, fmt.Errorf("序列化 manifest 失败: %w", err)
		}
		if err := w.WriteLine(b); err != nil {
			_ = w.Close()
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		return map[ResumeKey]bool{}, nil
	}

	file, err := Read(path)
	if err != nil {
		return nil, fmt.Errorf("续跑读回失败（文件可能损坏）: %w", err)
	}
	if file.Manifest.PlanDigest != manifest.PlanDigest {
		return nil, fmt.Errorf(
			"拒绝续写：文件中的 plan_digest (%s) 与本次采集计划 (%s) 不一致\n"+
				"  续写会得到混合两种采集计划的文件。请换输出文件或还原配置。",
			shortDigest(file.Manifest.PlanDigest), shortDigest(manifest.PlanDigest))
	}

	done = map[ResumeKey]bool{}
	var keep []Record
	for _, r := range file.Records {
		if r.Status == "success" {
			done[ResumeKeyOf(r)] = true
		}
		keep = append(keep, r)
	}

	if file.Aggregates != nil {
		// 去掉旧 aggregates 尾行重写一遍（后续采集会重算 aggregates）
		tmp := path + ".tmp"
		w, err := Append(tmp)
		if err != nil {
			return nil, err
		}
		b, _ := json.Marshal(file.Manifest)
		if err := w.WriteLine(b); err != nil {
			_ = w.Close()
			return nil, err
		}
		for _, r := range keep {
			if err := w.WriteRecord(r); err != nil {
				_ = w.Close()
				return nil, err
			}
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		if err := os.Rename(tmp, path); err != nil {
			return nil, fmt.Errorf("重写 rawData 失败: %w", err)
		}
	}
	return done, nil
}

func shortDigest(d string) string {
	if len(d) > 20 {
		return d[:20] + "…"
	}
	return d
}

// ResumeKey 续跑键：(probe, question, bucket, protocol, transport, repeat)。
// attempt 不参与 —— 任一 attempt 成功即视为该请求完成。
type ResumeKey struct {
	ProbeID       string
	QuestionID    string
	ContextBucket int
	Protocol      string
	TransportMode string
	RepeatIndex   int
}

// ResumeKeyOf 从 record 提取续跑键。
func ResumeKeyOf(r Record) ResumeKey {
	return ResumeKey{
		ProbeID:       r.ProbeID,
		QuestionID:    r.QuestionID,
		ContextBucket: r.ContextBucket,
		Protocol:      r.Protocol,
		TransportMode: r.TransportMode,
		RepeatIndex:   r.RepeatIndex,
	}
}

// WriteAggregates 在文件末尾追加 aggregates 行（新 gzip 成员）。
func WriteAggregates(path string, records []Record) error {
	agg := BuildAggregates(records)
	b, err := json.Marshal(agg)
	if err != nil {
		return err
	}
	w, err := Append(path)
	if err != nil {
		return err
	}
	if err := w.WriteLine(b); err != nil {
		_ = w.Close()
		return err
	}
	return w.Close()
}

// BuildAggregates 从全部 record 重算 aggregates。
// aggregates 是派生数据：与 record 不一致时以 record 为准（SPEC-RAWDATA §5）。
func BuildAggregates(records []Record) Aggregates {
	agg := Aggregates{
		Kind:        "aggregates",
		RecordCount: len(records),
		Cells:       map[string]CellAggregate{},
		Transport:   buildTransport(records),
	}
	for _, r := range records {
		key := CellKeyOf(r)
		ca := agg.Cells[key]
		ca.N++
		if r.Status == "success" {
			ca.NValid++
			// 探针特有摘要（派生数据，仅供人看；判定一律走 record）
			switch r.ProbeID {
			case "onetoken":
				var o struct {
					Value string `json:"value"`
				}
				if json.Unmarshal(r.Observation, &o) == nil && o.Value != "" {
					if ca.Distribution == nil {
						ca.Distribution = map[string]int{}
					}
					ca.Distribution[o.Value]++
				}
			case "tokenizer":
				var o struct {
					PromptTokens int `json:"prompt_tokens"`
				}
				if json.Unmarshal(r.Observation, &o) == nil && o.PromptTokens > 0 {
					ca.Values = append(ca.Values, o.PromptTokens)
				}
			case "toolcall":
				var o struct {
					Tool string `json:"tool"`
				}
				if json.Unmarshal(r.Observation, &o) == nil && o.Tool != "" {
					if ca.Distribution == nil {
						ca.Distribution = map[string]int{}
					}
					ca.Distribution[o.Tool]++
				}
			case "think-effort":
				var o struct {
					ReasoningValue int `json:"reasoning_value"`
				}
				if json.Unmarshal(r.Observation, &o) == nil && o.ReasoningValue >= 0 {
					ca.Values = append(ca.Values, o.ReasoningValue)
				}
			}
		}
		agg.Cells[key] = ca
	}
	return agg
}

// CellKeyOf cell 键：cell_key 各分量以 | 连接。
// tokenizer 含 protocol；think-effort 含 thinking_effort（按强度分组）与
// protocol（按协议分层，保证 cell 内观测单位一致，见 probe/thinkeffort.go）；
// 其余为 question_id + context_bucket。与 collection_plan 的 cell_key 一致，
// 保证同键的 record 落到同一个聚合 cell。
func CellKeyOf(r Record) string {
	switch r.ProbeID {
	case "tokenizer":
		return strings.Join([]string{r.ProbeID, r.QuestionID, itoa(r.ContextBucket), r.Protocol}, "|")
	case "think-effort":
		effort := "null"
		if r.ThinkingEffort != nil {
			effort = *r.ThinkingEffort
		}
		return strings.Join([]string{r.ProbeID, r.QuestionID, itoa(r.ContextBucket), effort, r.Protocol}, "|")
	default:
		return strings.Join([]string{r.ProbeID, r.QuestionID, itoa(r.ContextBucket)}, "|")
	}
}

func itoa(i int) string { return fmt.Sprintf("%d", i) }

// —— transport 指标聚合（描述性，不参与判定）——

func buildTransport(records []Record) Transport {
	t := Transport{
		ByProtocol:          map[string]TransportMetrics{},
		ByTransportMode:     map[string]TransportMetrics{},
		ByProtocolTransport: map[string]TransportMetrics{},
	}
	accs := map[string]*metricsAcc{}
	overall := &metricsAcc{}
	for _, r := range records {
		overall.add(r)
		for _, k := range []string{
			groupKey("proto", r.Protocol),
			groupKey("mode", r.TransportMode),
			groupKey("pt", r.Protocol, r.TransportMode),
		} {
			acc := accs[k]
			if acc == nil {
				acc = &metricsAcc{}
				accs[k] = acc
			}
			acc.add(r)
		}
	}
	t.Overall = overall.metrics()
	for k, a := range accs {
		parts := strings.SplitN(k, "\x00", 3)
		m := a.metrics()
		switch parts[0] {
		case "proto":
			t.ByProtocol[parts[1]] = m
		case "mode":
			t.ByTransportMode[parts[1]] = m
		case "pt":
			t.ByProtocolTransport[parts[1]+"\x00"+parts[2]] = m
		}
	}
	// pt 键转成 "protocol|mode" 形式
	fixed := map[string]TransportMetrics{}
	for k, v := range t.ByProtocolTransport {
		fixed[strings.ReplaceAll(k, "\x00", "|")] = v
	}
	t.ByProtocolTransport = fixed
	return t
}

func groupKey(kind string, parts ...string) string {
	return kind + "\x00" + strings.Join(parts, "\x00")
}

// metricsAcc 分位数计算的中介。
type metricsAcc struct {
	n, nErr, nTimeout int
	latency           []float64
	ttft              []float64
	tps               []float64
}

func (a *metricsAcc) add(r Record) {
	a.n++
	if r.Status != "success" {
		a.nErr++
	}
	if r.ErrorKind != nil && *r.ErrorKind == "timeout" {
		a.nTimeout++
	}
	a.latency = append(a.latency, float64(r.LatencyMs))
	if r.TtftMs != nil {
		a.ttft = append(a.ttft, float64(*r.TtftMs))
	}
	if r.Tps != nil {
		a.tps = append(a.tps, *r.Tps)
	}
}

func (a *metricsAcc) metrics() TransportMetrics {
	m := TransportMetrics{
		Availability: 1,
		ErrorRate:    0,
		TimeoutRate:  0,
		LatencyMs:    quantiles(a.latency, "p50", "p90", "p99"),
		TtftMs:       quantiles(a.ttft, "p50", "p90", "p99"),
		Tps:          quantiles(a.tps, "p50", "p90"),
	}
	if a.n > 0 {
		m.Availability = round3(float64(a.n-a.nErr) / float64(a.n))
		m.ErrorRate = round3(float64(a.nErr) / float64(a.n))
		m.TimeoutRate = round3(float64(a.nTimeout) / float64(a.n))
	}
	return m
}

// quantiles 排序后按名字取分位。输入为空返回 nil map。
func quantiles(vals []float64, names ...string) map[string]float64 {
	if len(vals) == 0 {
		return nil
	}
	sorted := append([]float64(nil), vals...)
	sort.Float64s(sorted)
	out := map[string]float64{}
	for i, name := range names {
		switch name {
		case "p50":
			out[name] = round3(stats.Quantile(sorted, 0.50))
		case "p90":
			out[name] = round3(stats.Quantile(sorted, 0.90))
		case "p99":
			_ = i
			out[name] = round3(stats.Quantile(sorted, 0.99))
		}
	}
	return out
}

func round3(f float64) float64 {
	if math.IsNaN(f) {
		return 0
	}
	return math.Round(f*1000) / 1000
}

// TempFilePath 生成 run 子命令的临时 rawData 路径（落在系统临时目录）。
func TempFilePath(prefix string) (string, error) {
	dir, err := os.MkdirTemp("", "tv-run-*")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, prefix+".rawdata.jsonl.gz"), nil
}
