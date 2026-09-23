package main

// progress 进度条的单元测试。

import (
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 进度条用 \r 原地重绘单行：不含换行、含 done/total、含块形字符。
func TestProgressBarDraw(t *testing.T) {
	var buf strings.Builder
	p := newProgressBar(&buf, 100)
	p.minInterval = 0 // 测试里关掉节流
	p.update(42)
	p.draw()

	out := buf.String()
	if !strings.HasPrefix(out, "\r") {
		t.Errorf("应以 \\r 开头（原地重绘）: %q", out)
	}
	if strings.Contains(out, "\n") {
		t.Errorf("绘制中不应含换行（换行是 Finish 的事）: %q", out)
	}
	if !strings.Contains(out, "42/100") {
		t.Errorf("应含 done/total: %q", out)
	}
	if !strings.Contains(out, "█") {
		t.Errorf("应含块形进度字符: %q", out)
	}
}

// 节流：两次绘制间隔小于 minInterval 时第二次跳过。
func TestProgressBarThrottle(t *testing.T) {
	var buf strings.Builder
	p := newProgressBar(&buf, 100)
	p.minInterval = time.Hour // 大到必然触发节流
	p.update(1)
	p.draw()
	n1 := buf.Len()
	p.update(2)
	p.draw() // 应被节流掉
	if buf.Len() != n1 {
		t.Errorf("节流期间不应重绘: %d -> %d", n1, buf.Len())
	}
	// 强制绘制（最后一次）不受节流限制
	p.update(3)
	p.drawForce()
	if !strings.Contains(buf.String(), "3/100") {
		t.Errorf("强制绘制应生效: %q", buf.String())
	}
}

// Erase 用空格抹掉当前行内容再回到行首，供日志行先擦后写。
func TestProgressBarErase(t *testing.T) {
	var buf strings.Builder
	p := newProgressBar(&buf, 100)
	p.minInterval = 0
	p.update(42)
	p.draw()
	before := buf.Len()

	p.erase()
	out := buf.String()[before:]
	if !strings.HasPrefix(out, "\r") || !strings.HasSuffix(out, "\r") {
		t.Errorf("擦除应以 \\r 开头并以 \\r 收尾（回到行首）: %q", out)
	}
	if !strings.Contains(out, "    ") {
		t.Errorf("擦除应写入空格覆盖条形: %q", out)
	}
	// 擦除后 next 行应为干净行首：再画一次，内容不应叠在旧条上
	p.draw()
	all := buf.String()
	if strings.Count(all, "42/100") != 2 {
		t.Errorf("42/100 应出现恰好 2 次（画、擦、重画）: %q", all)
	}
}

// Finish 结束进度条：画最终态并换行，后续日志从干净的下一行开始。
func TestProgressBarFinish(t *testing.T) {
	var buf strings.Builder
	p := newProgressBar(&buf, 100)
	p.minInterval = 0
	p.update(100)
	p.finish()

	out := buf.String()
	if !strings.Contains(out, "100/100") {
		t.Errorf("终态应含 100/100: %q", out)
	}
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("Finish 应以换行结束: %q", out)
	}
	// finish 之后再 draw 不应有任何输出
	before := buf.Len()
	p.update(101)
	p.drawForce()
	if buf.Len() != before {
		t.Error("finish 后进度条应封笔")
	}
}

// 零任务（续跑全部完成）不应画任何东西。
func TestProgressBarZeroTotal(t *testing.T) {
	var buf strings.Builder
	p := newProgressBar(&buf, 0)
	p.minInterval = 0
	p.drawForce()
	if buf.Len() != 0 {
		t.Errorf("total=0 不应输出: %q", buf.String())
	}
}

// 日志与进度条的协调：日志写出前必须先擦掉条形，否则日志会叠在条形后面。
func TestProgressBarLogCoordination(t *testing.T) {
	var buf strings.Builder
	p := newProgressBar(&buf, 100)
	p.minInterval = 0
	p.update(42)
	p.draw()
	before := buf.Len()

	// 日志经由 bar 包装的 writer 写出
	w := p.wrapForLog(&buf)
	_, _ = w.Write([]byte("WARN  请求失败 error_kind=timeout\n"))

	added := buf.String()[before:]
	// 先擦（\r + 空格 + \r），再是日志行
	if !strings.HasPrefix(added, "\r") || !strings.Contains(added, "WARN  请求失败") {
		t.Errorf("日志前应先擦条形再写日志: %q", added)
	}
	// 日志写完后条形可以重画
	p.draw()
	if !strings.HasSuffix(buf.String(), "42/100]") && !strings.Contains(buf.String(), "42/100") {
		t.Errorf("日志后应能重画条形: %q", buf.String())
	}
}

// 未开 --progress 时 stderr 不应出现 \r（进度条完全关闭）。
func TestCLICollectNoProgressByDefault(t *testing.T) {
	os.Setenv("TV_CLI_KEY", "sk-cli")
	defer os.Unsetenv("TV_CLI_KEY")
	srv := httptest.NewServer(&fakeChatEP{})
	defer srv.Close()
	cfgPath, _ := writeCollectFixture(t, srv.URL)

	orig := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	code := run([]string{"collect", "-c", cfgPath, "-o", filepath.Join(t.TempDir(), "o.gz")})
	_ = w.Close()
	os.Stderr = orig
	out, _ := io.ReadAll(r)

	if code != exitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if strings.Contains(string(out), "\r") {
		t.Errorf("默认不应有进度条输出（\r）:\n%s", out)
	}
}

// --progress 开启时 stderr 出现原地重绘的进度条，终态为 N/N。
func TestCLICollectProgress(t *testing.T) {
	os.Setenv("TV_CLI_KEY", "sk-cli")
	defer os.Unsetenv("TV_CLI_KEY")
	srv := httptest.NewServer(&fakeChatEP{})
	defer srv.Close()
	cfgPath, _ := writeCollectFixture(t, srv.URL)

	orig := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	out := filepath.Join(t.TempDir(), "o.gz")
	code := run([]string{"collect", "-c", cfgPath, "-o", out, "--progress"})
	_ = w.Close()
	os.Stderr = orig
	serr, _ := io.ReadAll(r)

	if code != exitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	s := string(serr)
	if !strings.Contains(s, "\r") {
		t.Errorf("--progress 应用 \r 原地重绘:\n%q", s)
	}
	if !strings.Contains(s, "16/16") {
		t.Errorf("终态应为 16/16（fixture 共 16 请求）:\n%q", s)
	}
	if !strings.Contains(s, "█") {
		t.Errorf("应含块形进度字符:\n%q", s)
	}
}
