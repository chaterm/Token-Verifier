package logging

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestParseLevel(t *testing.T) {
	cases := []struct {
		in   string
		want slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
	}
	for _, c := range cases {
		got, err := ParseLevel(c.in)
		if err != nil {
			t.Errorf("ParseLevel(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseLevel(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	if _, err := ParseLevel("bogus"); err == nil {
		t.Error("ParseLevel(\"bogus\") 应报错")
	}
	if _, err := ParseLevel(""); err == nil {
		t.Error("ParseLevel(\"\") 应报错")
	}
}

func TestSetupFiltersByLevel(t *testing.T) {
	var buf bytes.Buffer
	logger := Setup(&buf, slog.LevelWarn)
	logger.Debug("d 消息")
	logger.Info("i 消息")
	logger.Warn("w 消息")
	logger.Error("e 消息")

	out := buf.String()
	if strings.Contains(out, "d 消息") || strings.Contains(out, "i 消息") {
		t.Errorf("warn 级别不应输出 debug/info: %q", out)
	}
	if !strings.Contains(out, "w 消息") || !strings.Contains(out, "e 消息") {
		t.Errorf("warn 级别应输出 warn/error: %q", out)
	}
}

func TestSetupFormat(t *testing.T) {
	var buf bytes.Buffer
	logger := Setup(&buf, slog.LevelDebug)
	logger.Info("采集开始", "requests", 26)

	line := buf.String()
	// 与现有 CLI 输出同风格：级别名左对齐 + 消息 + key=value
	if !strings.HasPrefix(line, "INFO ") {
		t.Errorf("应以级别名开头: %q", line)
	}
	if !strings.Contains(line, "采集开始") {
		t.Errorf("应含消息: %q", line)
	}
	if !strings.Contains(line, "requests=26") {
		t.Errorf("应含属性: %q", line)
	}
	if !strings.HasSuffix(line, "\n") {
		t.Errorf("应以换行结尾: %q", line)
	}
	if strings.Contains(line, "time=") {
		t.Errorf("不应含 slog 默认时间字段: %q", line)
	}
}

func TestSetupErrorLevel(t *testing.T) {
	var buf bytes.Buffer
	logger := Setup(&buf, slog.LevelError)
	logger.Warn("w 消息")
	if buf.Len() != 0 {
		t.Errorf("error 级别不应输出 warn: %q", buf.String())
	}
}

func TestSanitizeTerminal(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"干净文本原样返回", "HTTP 401: invalid_api_key", "HTTP 401: invalid_api_key"},
		{"ESC 转义序列剥成空格", "before\x1b[31mRED\x1b[0mafter", "before [31mRED [0mafter"},
		{"换行与制表剥成空格", "line1\nline2\tcol", "line1 line2 col"},
		{"回车剥成空格（防单行重绘被伪造）", "real\rfake", "real fake"},
		{"DEL 剥成空格", "a\x7fb", "a b"},
		{"多字节字符保留", "错误：密钥无效 ✓", "错误：密钥无效 ✓"},
		{"空串", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SanitizeTerminal(c.in); got != c.want {
				t.Errorf("SanitizeTerminal(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// 端点返回的错误详情落到终端前必须已被剥控制字符：
// 恶意端点可用 ANSI 序列伪造终端显示（清屏、改色、覆盖上一行）。
func TestSanitizeTerminalGuardsLoggedDetail(t *testing.T) {
	var buf bytes.Buffer
	logger := Setup(&buf, slog.LevelWarn)
	logger.Warn("请求失败", "detail", SanitizeTerminal("evil\x1b[2J\x1b[Hcleared"))
	out := buf.String()
	if strings.ContainsAny(strings.TrimSuffix(out, "\n"), "\x1b\r\n") {
		t.Errorf("日志行不应残留 ESC/CR/LF: %q", out)
	}
	if strings.Count(out, "\n") != 1 {
		t.Errorf("应恰好一行（换行被剥成空格）: %q", out)
	}
}
