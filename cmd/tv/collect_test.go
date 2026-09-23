package main

// collect / run 的端到端 CLI 测试：httptest 假端点，不打真实 API。

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/chaterm/token-verifier/internal/collect"
)

// fakeChatEP 假 openai-chat 端点（与 internal/collect 的 fakeOpenAI 同型）。
type fakeChatEP struct {
	promptTokenShift int // > 0 时给 CJK prompt 的 prompt_tokens 加偏移
	requests         atomic.Int64
}

func (f *fakeChatEP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.requests.Add(1)
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &req)
	prompt := ""
	if len(req.Messages) > 0 {
		prompt = req.Messages[0].Content
	}
	pt := 50 + len(prompt)/4
	if f.promptTokenShift > 0 && hasCJK(prompt) {
		pt += f.promptTokenShift
	}
	completion := fmt.Sprintf("%d", 1+rand.New(rand.NewSource(int64(len(prompt)))).Intn(100))
	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", completion)
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":3}}\n\n", pt)
		fmt.Fprint(w, "data: [DONE]\n\n")
		return
	}
	w.WriteHeader(200)
	fmt.Fprintf(w, `{"choices":[{"message":{"content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":%d,"completion_tokens":3}}`, completion, pt)
}

func hasCJK(s string) bool {
	for _, r := range s {
		if r > 0x2E80 {
			return true
		}
	}
	return false
}

// writeCollectFixture 写配置 + suite 文件，返回两路径。
func writeCollectFixture(t *testing.T, baseURL string) (cfgPath, suitePath string) {
	t.Helper()
	dir := t.TempDir()
	suitePath = filepath.Join(dir, "suite.yaml")
	suiteYAML := `suite_version: v1
output_contract:
  field: answer
  system_prompt: '请只回答 JSON {"answer": "<your answer>"}'
probes:
  onetoken:
    normalize: number
    items:
      - id: ot.v1.001
        prompt: "说一个 1 到 100 的随机整数，只输出数字。"
  tokenizer:
    normalize: none
    items:
      - id: tk.v1.001
        prompt: "重复以下内容一次：こんにちは、🎉。"
      - id: tk.v1.002
        prompt: "重复以下内容一次：全角ＡＢＣ与中文混排。"
      - id: tk.v1.003
        prompt: "重复以下内容一次：한국어 日本語 混在。"
      - id: tk.v1.004
        prompt: "重复以下内容一次：𝔘𝔫𝔦𝔠𝔬𝔡𝔢 符号。"
      - id: tk.v1.005
        prompt: "重复以下内容一次：嵌套 {\"x\": [true, null]}"
      - id: tk.v1.006
        prompt: "重复以下内容一次：更长一点的中文填充文本行。"
`
	os.WriteFile(suitePath, []byte(suiteYAML), 0o644)

	cfgPath = filepath.Join(dir, "tv.yaml")
	cfgYAML := fmt.Sprintf(`version: 1
target:
  base_url: %s
  api_key_env: TV_CLI_KEY
  model: fake-model
  protocols:
    - id: openai-chat
      weight: 1.0
      transport: { stream: 0.5, non_stream: 0.5 }
suite:
  path: %s
probes:
  onetoken:
    enabled: true
    repeats: 10
    temperature: 1.0
    max_tokens: 16
    context_buckets: [0]
  tokenizer:
    enabled: true
    repeats: 1
    temperature: 0.0
    max_tokens: 16
    context_buckets: [0]
thresholds:
  onetoken: 0.9
  tokenizer: 0.01
runtime:
  max_concurrency: 4
  timeout_sec: 10
  max_attempts: 2
  seed: 42
  raw_level: digest
`, baseURL, filepath.ToSlash(suitePath))
	// yaml 里 Windows 反斜杠路径要加引号
	cfgYAML = strings.ReplaceAll(cfgYAML, "path: "+filepath.ToSlash(suitePath), "path: \""+filepath.ToSlash(suitePath)+"\"")
	os.WriteFile(cfgPath, []byte(cfgYAML), 0o644)
	return cfgPath, suitePath
}

func TestCLICollectAndRun(t *testing.T) {
	os.Setenv("TV_CLI_KEY", "sk-cli")
	defer os.Unsetenv("TV_CLI_KEY")

	srv := httptest.NewServer(&fakeChatEP{})
	defer srv.Close()
	cfgPath, _ := writeCollectFixture(t, srv.URL)

	// collect：产出 rawData
	out := filepath.Join(t.TempDir(), "cli.rawdata.jsonl.gz")
	code := run([]string{"collect", "-c", cfgPath, "-o", out})
	if code != exitOK {
		t.Fatalf("collect: exit = %d, want 0", code)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("rawData 未产出: %v", err)
	}

	// run：与自身采集的另一份比较（同端点 → 全 pass，exit 0）
	code = run([]string{"run", "-c", cfgPath, out})
	if code != exitOK {
		t.Errorf("run 同端点: exit = %d, want 0", code)
	}

	// run：与换分词器端点采集比较 → tokenizer fail → exit 1
	srvShift := httptest.NewServer(&fakeChatEP{promptTokenShift: 999})
	defer srvShift.Close()
	cfgShift, _ := writeCollectFixture(t, srvShift.URL)
	baseline := filepath.Join(t.TempDir(), "baseline.rawdata.jsonl.gz")
	if code := run([]string{"collect", "-c", cfgShift, "-o", baseline}); code != exitOK {
		t.Fatalf("collect shift: exit = %d", code)
	}
	code = run([]string{"run", "-c", cfgPath, baseline})
	if code != exitFail {
		t.Errorf("run 换分词器: exit = %d, want 1", code)
	}
}

func TestCLICollectDryRun(t *testing.T) {
	os.Setenv("TV_CLI_KEY", "sk-cli")
	defer os.Unsetenv("TV_CLI_KEY")
	// dry-run 不需要可达端点
	cfgPath, _ := writeCollectFixture(t, "http://unused.example.com")
	code := run([]string{"collect", "-c", cfgPath, "-o", "unused.gz", "--dry-run"})
	if code != exitOK {
		t.Errorf("dry-run: exit = %d, want 0", code)
	}
}

func TestCLICollectUnreachableEndpoint(t *testing.T) {
	os.Setenv("TV_CLI_KEY", "sk-cli")
	defer os.Unsetenv("TV_CLI_KEY")
	// 端口关闭的本地地址 → 全部 connection 失败 → exit 4
	cfgPath, _ := writeCollectFixture(t, "http://127.0.0.1:1")
	out := filepath.Join(t.TempDir(), "o.rawdata.jsonl.gz")
	code := run([]string{"collect", "-c", cfgPath, "-o", out})
	if code != exitCollect {
		t.Errorf("unreachable: exit = %d, want 4", code)
	}
}

// 采集摘要必须把「为什么失败」印出来：分类计数 + 端点原话 + 排查方向。
// 此前只有「成功 0 / 失败 131」，原因埋在 rawData 里没人看得到。
func TestRenderErrorBreakdown(t *testing.T) {
	res := &collect.Result{
		TotalRequests: 131, SuccessCount: 0, ErrorCount: 131,
		ErrorKinds: map[string]int{"http_4xx": 130, "timeout": 1},
		ErrorSamples: map[string]collect.ErrorSample{
			"http_4xx": {
				HTTPCode: 401,
				Detail:   `HTTP 401: {"error":{"message":"Incorrect API key provided: ***","code":"invalid_api_key"}}`,
				ProbeID:  "onetoken", Protocol: "openai-chat",
			},
			"timeout": {Detail: "context deadline exceeded", ProbeID: "needle", Protocol: "openai-chat"},
		},
	}
	var buf strings.Builder
	renderErrorBreakdown(&buf, res)
	out := buf.String()

	// 分类计数（按数量降序，多的在前）
	if !strings.Contains(out, "http_4xx") || !strings.Contains(out, "130") {
		t.Errorf("应含分类与计数:\n%s", out)
	}
	if i, j := strings.Index(out, "http_4xx"), strings.Index(out, "timeout"); i > j {
		t.Errorf("应按计数降序（http_4xx 130 在 timeout 1 之前）:\n%s", out)
	}
	// HTTP 状态码
	if !strings.Contains(out, "401") {
		t.Errorf("应含 http_code:\n%s", out)
	}
	// 端点原话
	if !strings.Contains(out, "invalid_api_key") {
		t.Errorf("应含端点返回的错误原话:\n%s", out)
	}
	// 可操作的排查方向：401 应指向密钥
	if !strings.Contains(out, "api_key_env") {
		t.Errorf("401 应提示检查密钥配置:\n%s", out)
	}
	// timeout 应指向超时配置
	if !strings.Contains(out, "timeout_sec") {
		t.Errorf("timeout 应提示检查超时配置:\n%s", out)
	}
}

// 全部失败时摘要不能说得像成功：必须显式点出「零成功样本」。
func TestRenderErrorBreakdownAllFailed(t *testing.T) {
	res := &collect.Result{
		TotalRequests: 20, SuccessCount: 0, ErrorCount: 20,
		ErrorKinds:   map[string]int{"http_4xx": 20},
		ErrorSamples: map[string]collect.ErrorSample{"http_4xx": {HTTPCode: 403, Detail: "quota exceeded"}},
	}
	var buf strings.Builder
	renderErrorBreakdown(&buf, res)
	out := buf.String()
	if !strings.Contains(out, "零成功样本") {
		t.Errorf("全失败应显式告警零成功样本:\n%s", out)
	}
}

// 无失败时不应印任何东西（成功路径保持干净）。
func TestRenderErrorBreakdownSilentOnSuccess(t *testing.T) {
	res := &collect.Result{TotalRequests: 20, SuccessCount: 20, ErrorCount: 0}
	var buf strings.Builder
	renderErrorBreakdown(&buf, res)
	if buf.Len() != 0 {
		t.Errorf("无失败不应有输出: %q", buf.String())
	}
}

// 端点原话经摘要渲染前必须剥掉控制字符（防 ANSI 伪造终端显示）。
func TestRenderErrorBreakdownSanitizes(t *testing.T) {
	res := &collect.Result{
		TotalRequests: 1, SuccessCount: 0, ErrorCount: 1,
		ErrorKinds: map[string]int{"http_4xx": 1},
		ErrorSamples: map[string]collect.ErrorSample{
			"http_4xx": {HTTPCode: 400, Detail: "evil\x1b[2J\x1b[H\rFAKE 成功"},
		},
	}
	var buf strings.Builder
	renderErrorBreakdown(&buf, res)
	if strings.ContainsAny(buf.String(), "\x1b\r") {
		t.Errorf("渲染结果残留 ESC/CR: %q", buf.String())
	}
}

func TestCLIRunPreflight(t *testing.T) {
	os.Setenv("TV_CLI_KEY", "sk-cli")
	defer os.Unsetenv("TV_CLI_KEY")
	// 端点不可达 + 基线不存在：预检应在采集之前拦下（exit 2，不发起任何请求）
	cfgPath, _ := writeCollectFixture(t, "http://127.0.0.1:1")
	baseline := filepath.Join(t.TempDir(), "missing.rawdata.jsonl.gz")
	code := run([]string{"run", "-c", cfgPath, baseline})
	if code != exitUsage {
		t.Errorf("基线不存在: exit = %d, want 2（预检先于采集）", code)
	}

	// 基线存在但计划不兼容：预检应报 exit 3，同样不发起采集
	fixture := writeFixture(t, "sha256:totally-different", twoProbePlan(), matchedRecords())
	code = run([]string{"run", "-c", cfgPath, fixture})
	if code != exitIncompat {
		t.Errorf("digest 不兼容: exit = %d, want 3（预检先于采集）", code)
	}
}

func TestCLIRunPreflightThreshold(t *testing.T) {
	os.Setenv("TV_CLI_KEY", "sk-cli")
	defer os.Unsetenv("TV_CLI_KEY")
	// 先用可达端点采一份基线
	srv := httptest.NewServer(&fakeChatEP{})
	defer srv.Close()
	cfgPath, _ := writeCollectFixture(t, srv.URL)
	baseline := filepath.Join(t.TempDir(), "baseline.rawdata.jsonl.gz")
	if code := run([]string{"collect", "-c", cfgPath, "-o", baseline}); code != exitOK {
		t.Fatalf("collect baseline: exit = %d", code)
	}
	// 去掉 thresholds 段的配置：digest 匹配但阈值缺失 → 预检 exit 2，不采集
	noThresholds := readAndStrip(t, cfgPath)
	code := run([]string{"run", "-c", noThresholds, baseline})
	if code != exitUsage {
		t.Errorf("缺阈值: exit = %d, want 2（预检先于采集）", code)
	}
}

// readAndStrip 复制配置并删掉 thresholds 段。
func readAndStrip(t *testing.T, cfgPath string) string {
	t.Helper()
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	var out []string
	skip := false
	for _, l := range lines {
		if strings.HasPrefix(l, "thresholds:") {
			skip = true
			continue
		}
		if skip && (strings.HasPrefix(l, " ") || strings.TrimSpace(l) == "") {
			if strings.TrimSpace(l) == "" {
				skip = false
				out = append(out, l)
			}
			continue
		}
		skip = false
		out = append(out, l)
	}
	p := filepath.Join(t.TempDir(), "no-thresholds.yaml")
	if err := os.WriteFile(p, []byte(strings.Join(out, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCLICollectBadConfig(t *testing.T) {
	os.Setenv("TV_CLI_KEY", "sk-cli")
	defer os.Unsetenv("TV_CLI_KEY")
	// 配置文件不存在 → exit 2
	code := run([]string{"collect", "-c", "nope.yaml", "-o", "out.gz"})
	if code != exitUsage {
		t.Errorf("bad config: exit = %d, want 2", code)
	}
	// 缺 api_key 环境变量 → exit 2
	os.Unsetenv("TV_CLI_KEY")
	srv := httptest.NewServer(&fakeChatEP{})
	defer srv.Close()
	cfgPath, _ := writeCollectFixture(t, srv.URL)
	code = run([]string{"collect", "-c", cfgPath, "-o", filepath.Join(t.TempDir(), "o.gz")})
	if code != exitUsage {
		t.Errorf("missing env: exit = %d, want 2", code)
	}
}

func TestCLIRunPreflightFormatVersion(t *testing.T) {
	os.Setenv("TV_CLI_KEY", "sk-cli")
	defer os.Unsetenv("TV_CLI_KEY")
	// 基线 format_version 与当前写入器不一致：预检应在采集之前拒绝（exit 3），
	// 即使 digest 恰好相等、即使 --allow-subset（格式版本任何模式都拒绝）
	cfgPath, _ := writeCollectFixture(t, "http://127.0.0.1:1")
	baseline := writeFixtureVersion(t, 2, "sha256:x", twoProbePlan(), matchedRecords())

	code := run([]string{"run", "-c", cfgPath, baseline})
	if code != exitIncompat {
		t.Errorf("exit = %d, want 3（format_version 预检先于采集）", code)
	}
	code = run([]string{"run", "-c", cfgPath, "--allow-subset", baseline})
	if code != exitIncompat {
		t.Errorf("--allow-subset: exit = %d, want 3（格式版本子集模式也拒绝）", code)
	}
}

// writeFixtureVersion 同 writeFixture，但可指定 format_version。
func writeFixtureVersion(t *testing.T, fv int, digest string, plan map[string]any, records []map[string]any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "f.rawdata.jsonl.gz")
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := gzip.NewWriter(out)
	enc := json.NewEncoder(w)
	mustEncode(t, enc, map[string]any{
		"kind": "manifest", "format_version": fv,
		"plan_digest": digest, "collection_plan": plan,
	})
	for _, r := range records {
		r["kind"] = "record"
		mustEncode(t, enc, r)
	}
	mustEncode(t, enc, map[string]any{"kind": "aggregates", "record_count": len(records)})
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}
