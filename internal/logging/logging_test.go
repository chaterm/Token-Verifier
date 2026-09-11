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
