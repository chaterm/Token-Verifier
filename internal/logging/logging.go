// Package logging 提供 CLI 的统一日志初始化：解析 --log-level、
// 构造与现有输出风格（"WARN  xxx"）一致的 slog.Logger。
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
)

// Levels 列出 --log-level 接受的值，用于 flag 帮助与报错提示。
var Levels = []string{"debug", "info", "warn", "error"}

// ParseLevel 把 --log-level 的字符串值解析为 slog.Level（大小写不敏感）。
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("非法日志级别 %q，可选: %s", s, strings.Join(Levels, "|"))
	}
}

// Setup 构造写到 w 的 logger。输出形如：
//
//	INFO  采集开始 requests=26
//
// 级别名左对齐 5 字符（与 CLI 现有 "WARN  /ERROR  " 前缀风格一致），
// 不带时间戳 —— 一次性验证工具，时间信息由 rawData 与报告承担。
func Setup(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(&handler{w: w, level: level})
}

// handler 极简文本 handler："LEVEL msg key=value ..."，一行一条。
type handler struct {
	w     io.Writer
	level slog.Level
	attrs []slog.Attr
	mu    *sync.Mutex
}

func (h *handler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

func (h *handler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%-5s %s", r.Level.String(), r.Message)
	for _, a := range h.attrs {
		writeAttr(&b, a)
	}
	r.Attrs(func(a slog.Attr) bool {
		writeAttr(&b, a)
		return true
	})
	b.WriteByte('\n')

	mu := h.mu
	if mu == nil {
		mu = &sync.Mutex{}
	}
	mu.Lock()
	defer mu.Unlock()
	_, err := io.WriteString(h.w, b.String())
	return err
}

func writeAttr(b *strings.Builder, a slog.Attr) {
	if a.Equal(slog.Attr{}) {
		return
	}
	// 错误值走 Error() 而不是 %+v，避免打出内部结构
	if a.Value.Kind() == slog.KindAny {
		if err, ok := a.Value.Any().(error); ok {
			a.Value = slog.StringValue(err.Error())
		}
	}
	fmt.Fprintf(b, " %s=%s", a.Key, a.Value.String())
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	mu := h.mu
	if mu == nil {
		mu = &sync.Mutex{}
	}
	return &handler{
		w:     h.w,
		level: h.level,
		attrs: append(append([]slog.Attr{}, h.attrs...), attrs...),
		mu:    mu,
	}
}

func (h *handler) WithGroup(name string) slog.Handler {
	// CLI 日志不用 group；属性直接带前缀拼平
	mu := h.mu
	if mu == nil {
		mu = &sync.Mutex{}
	}
	prefixed := make([]slog.Attr, len(h.attrs))
	for i, a := range h.attrs {
		prefixed[i] = slog.Attr{Key: name + "." + a.Key, Value: a.Value}
	}
	return &handler{w: h.w, level: h.level, attrs: prefixed, mu: mu}
}
