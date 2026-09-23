package main

// progress 进度条：--progress 显式开启时，向 stderr 画一条 \r 原地重绘的单行。
// 不用 ANSI 光标控制：重定向到文件时最坏只是多几个 \r 分隔的片段，不会留下
// 一堆转义序列。与日志行共享一把锁：日志写之前先 erase 擦掉条形，写完日志
// 下一次 draw 再画回来。

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// progressBar 单行进度条。全部方法并发安全。
type progressBar struct {
	w     io.Writer
	total int

	mu          sync.Mutex
	done        int
	lastWidth   int           // 上次画出的行宽（erase 用空格覆盖这么长）
	lastDraw    time.Time     // 上次画出时间（节流用）
	minInterval time.Duration // 两次重绘的最小间隔；0 = 不节流
	finished    bool
}

// progressMinInterval 默认重绘节流间隔：高频完成时（小请求、限流低）不刷爆终端。
const progressMinInterval = 100 * time.Millisecond

// progressBarWidth 块形条的字符宽度（与 verbose 报告的 ASCII 条形风格一致）。
const progressBarWidth = 20

// newProgressBar 构造进度条；total=0（如续跑全部完成）时一切绘制方法都是 no-op。
func newProgressBar(w io.Writer, total int) *progressBar {
	return &progressBar{w: w, total: total, minInterval: progressMinInterval}
}

// update 更新完成数；不触发绘制（绘制由 draw/drawForce 显式触发）。
func (p *progressBar) update(done int) {
	p.mu.Lock()
	p.done = done
	p.mu.Unlock()
}

// setTotal 设置总任务数。总数在采集计划展开后才知道（经首个进度回调传入），
// 只设一次；total=0（续跑全部完成）保持 no-op 语义。
func (p *progressBar) setTotal(total int) {
	p.mu.Lock()
	if p.total == 0 {
		p.total = total
	}
	p.mu.Unlock()
}

// draw 节流重绘：距上次不足 minInterval 时跳过。
func (p *progressBar) draw() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.finished || p.total == 0 {
		return
	}
	if p.minInterval > 0 && time.Since(p.lastDraw) < p.minInterval {
		return
	}
	p.paintLocked()
}

// drawForce 不受节流地重绘（最后一次、以及测试用）。
func (p *progressBar) drawForce() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.finished || p.total == 0 {
		return
	}
	p.paintLocked()
}

// paintLocked 画一行：\r + 内容（无换行）。调用方须持锁。
func (p *progressBar) paintLocked() {
	filled := 0
	if p.total > 0 {
		filled = p.done * progressBarWidth / p.total
		if p.done > 0 && filled == 0 {
			filled = 1 // 有进展就至少亮一格
		}
	}
	line := fmt.Sprintf("%d/%d [%s%s]",
		p.done, p.total,
		strings.Repeat("█", filled),
		strings.Repeat("░", progressBarWidth-filled))
	fmt.Fprintf(p.w, "\r%s", line)
	p.lastWidth = len(line)
	p.lastDraw = time.Now()
}

// erase 抹掉当前行内容并回到行首。日志行写出前调用，避免日志叠在条形上。
func (p *progressBar) erase() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lastWidth == 0 {
		return
	}
	fmt.Fprintf(p.w, "\r%s\r", strings.Repeat(" ", p.lastWidth))
	p.lastWidth = 0
}

// wrapForLog 返回一个 writer：写入前先在 bar 锁内擦掉条形再写。
// 给日志 handler 用，保证日志行落在干净的行首而不是叠在条形后面。
// 擦+写在同一把 bar 锁内完成，绘制方无法插进中间。
func (p *progressBar) wrapForLog(w io.Writer) io.Writer {
	return &eraseWriter{bar: p, inner: w}
}

type eraseWriter struct {
	bar   *progressBar
	inner io.Writer
}

func (e *eraseWriter) Write(b []byte) (int, error) {
	e.bar.mu.Lock()
	defer e.bar.mu.Unlock()
	if e.bar.lastWidth > 0 {
		fmt.Fprintf(e.inner, "\r%s\r", strings.Repeat(" ", e.bar.lastWidth))
		e.bar.lastWidth = 0
	}
	return e.inner.Write(b)
}

// finish 画终态并换行，封笔。之后 draw/drawForce 均为 no-op。
func (p *progressBar) finish() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.finished || p.total == 0 {
		return
	}
	p.paintLocked()
	fmt.Fprintln(p.w)
	p.finished = true
}
