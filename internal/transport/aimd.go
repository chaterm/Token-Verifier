package transport

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ConcurrencySnapshot 运行期与收尾时的并发度快照。
type ConcurrencySnapshot struct {
	CurrentCwnd    int
	PeakCwnd       int
	MaxConcurrency int
	InFlight       int
	ThrottleEvents int
}

// ConcurrencyEvent AIMD 并发窗口变化事件，供上层（如 TV-Watch）实时观测。
type ConcurrencyEvent struct {
	Cwnd           int
	InFlight       int
	PeakCwnd       int
	ThrottleEvents int
	Reason         string // init | probe_up | throttle:429 | throttle:timeout | throttle:http_5xx | throttle:connection
}

// AdaptiveLimiter AIMD（慢启动 + 加性增 / 乘性减）动态并发与限流退避控制器。
//
// 当 enabled=false 时退化为固定上限信号量（cwnd 恒等于 maxLimit），
// 但服务端显式返回的 Retry-After 冷却窗口仍然生效。
type AdaptiveLimiter struct {
	enabled  bool
	maxLimit int
	debounce time.Duration
	onEvent  func(ConcurrencyEvent)

	mu             sync.Mutex
	cwnd           float64
	ssthresh       float64
	inFlight       int
	peakCwnd       int
	throttleEvents int
	cooldownUntil  time.Time
	lastBackoffAt  time.Time
	waitCh         chan struct{}
}

const defaultAIMDDebounce = 250 * time.Millisecond

// NewAdaptiveLimiter 构造并发控制器。
func NewAdaptiveLimiter(enabled bool, initial, maxLimit int, debounce time.Duration, onEvent func(ConcurrencyEvent)) *AdaptiveLimiter {
	if maxLimit < 1 {
		maxLimit = 1
	}
	if initial < 1 {
		initial = minInt(2, maxLimit)
	}
	if initial > maxLimit {
		initial = maxLimit
	}
	if !enabled {
		initial = maxLimit
	}
	if debounce <= 0 {
		debounce = defaultAIMDDebounce
	}
	l := &AdaptiveLimiter{
		enabled:  enabled,
		maxLimit: maxLimit,
		debounce: debounce,
		onEvent:  onEvent,
		cwnd:     float64(initial),
		ssthresh: float64(maxLimit),
		peakCwnd: initial,
		waitCh:   make(chan struct{}),
	}
	if onEvent != nil {
		onEvent(ConcurrencyEvent{
			Cwnd:     initial,
			InFlight: 0,
			PeakCwnd: initial,
			Reason:   "init",
		})
	}
	return l
}

// Acquire 获取一个并发槽位；若处于 429 冷却期或已满窗则阻塞等待，ctx 取消立即返回。
func (l *AdaptiveLimiter) Acquire(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		l.mu.Lock()
		now := time.Now()
		if wait := l.cooldownUntil.Sub(now); wait > 0 {
			ch := l.waitCh
			l.mu.Unlock()
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-ch:
				timer.Stop()
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			}
			continue
		}

		limit := l.limitLocked()
		if l.inFlight < limit {
			l.inFlight++
			l.mu.Unlock()
			return nil
		}
		ch := l.waitCh
		l.mu.Unlock()

		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Release 归还并发槽位并根据本次请求结果更新 AIMD 拥塞窗口。
// ctxCanceled 为 true（外部主动取消任务）时不视作端点拥塞信号。
func (l *AdaptiveLimiter) Release(res Result, fallbackCooldown time.Duration, ctxCanceled bool) {
	var emit *ConcurrencyEvent

	l.mu.Lock()
	prevInFlight := l.inFlight
	if l.inFlight > 0 {
		l.inFlight--
	}
	prevLimit := l.limitLocked()
	now := time.Now()

	is429 := res.HTTPCode == http.StatusTooManyRequests
	isCongestion := !ctxCanceled && (is429 ||
		res.ErrorKind == "timeout" ||
		res.ErrorKind == "http_5xx" ||
		res.ErrorKind == "connection")

	if isCongestion {
		// 429 冷却期：优先采用服务端 Retry-After；无头且开启自适应时采用短暂退避冷却，
		// 阻止其它 worker 在同一毫秒继续冲撞已被限流的网关。
		if is429 {
			pause := res.RetryAfter
			if pause <= 0 && l.enabled {
				pause = fallbackCooldown
			}
			if pause > 0 {
				until := now.Add(pause)
				if until.After(l.cooldownUntil) {
					l.cooldownUntil = until
				}
			}
		}

		if l.enabled && (l.lastBackoffAt.IsZero() || now.Sub(l.lastBackoffAt) >= l.debounce) {
			l.lastBackoffAt = now
			l.throttleEvents++
			next := math.Max(1.0, math.Floor(l.cwnd*0.5))
			l.ssthresh = next
			l.cwnd = next
			reason := "throttle:" + res.ErrorKind
			if is429 {
				reason = "throttle:429"
			}
			emit = &ConcurrencyEvent{
				Cwnd:           l.limitLocked(),
				InFlight:       l.inFlight,
				PeakCwnd:       l.peakCwnd,
				ThrottleEvents: l.throttleEvents,
				Reason:         reason,
			}
		}
	} else if res.ErrorKind == "" && l.enabled && !now.Before(l.cooldownUntil) {
		// 仅当在途请求确实触及当前窗口附近时才上探，避免空闲期虚增窗口。
		wasUtilized := prevInFlight >= maxInt(1, prevLimit-1)
		if wasUtilized && l.cwnd < float64(l.maxLimit) {
			if l.cwnd < l.ssthresh {
				// 慢启动：每 2 个成功响应 +1 并发，快速逼近可用上限
				l.cwnd = math.Min(float64(l.maxLimit), l.cwnd+0.5)
			} else {
				// 拥塞避免：每满一窗成功响应 +1 并发，温和试探恢复
				l.cwnd = math.Min(float64(l.maxLimit), l.cwnd+1.0/l.cwnd)
			}
			newLimit := l.limitLocked()
			if newLimit > l.peakCwnd {
				l.peakCwnd = newLimit
			}
			if newLimit != prevLimit {
				emit = &ConcurrencyEvent{
					Cwnd:           newLimit,
					InFlight:       l.inFlight,
					PeakCwnd:       l.peakCwnd,
					ThrottleEvents: l.throttleEvents,
					Reason:         "probe_up",
				}
			}
		}
	}

	l.broadcastLocked()
	l.mu.Unlock()

	if emit != nil && l.onEvent != nil {
		l.onEvent(*emit)
	}
}

// Snapshot 返回当前并发控制状态。
func (l *AdaptiveLimiter) Snapshot() ConcurrencySnapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	return ConcurrencySnapshot{
		CurrentCwnd:    l.limitLocked(),
		PeakCwnd:       l.peakCwnd,
		MaxConcurrency: l.maxLimit,
		InFlight:       l.inFlight,
		ThrottleEvents: l.throttleEvents,
	}
}

func (l *AdaptiveLimiter) limitLocked() int {
	n := int(math.Floor(l.cwnd))
	if n < 1 {
		return 1
	}
	if n > l.maxLimit {
		return l.maxLimit
	}
	return n
}

func (l *AdaptiveLimiter) broadcastLocked() {
	close(l.waitCh)
	l.waitCh = make(chan struct{})
}

// parseRetryAfter 解析 HTTP Retry-After 响应头（支持秒数、浮点秒或 HTTP-date），
// 并受 maxDur 封顶。无法解析或 <= 0 时返回 0。
func parseRetryAfter(val string, now time.Time, maxDur time.Duration) time.Duration {
	val = strings.TrimSpace(val)
	if val == "" {
		return 0
	}
	var d time.Duration
	if secs, err := strconv.ParseFloat(val, 64); err == nil {
		if secs <= 0 {
			return 0
		}
		if secs > 3600 {
			secs = 3600
		}
		d = time.Duration(secs * float64(time.Second))
	} else if t, err := http.ParseTime(val); err == nil {
		d = t.Sub(now)
		if d <= 0 {
			return 0
		}
	} else {
		return 0
	}
	if maxDur > 0 && d > maxDur {
		d = maxDur
	}
	return d
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
