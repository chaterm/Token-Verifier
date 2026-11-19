package transport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	if got := parseRetryAfter("3", now, 10*time.Second); got != 3*time.Second {
		t.Errorf("seconds: got %v, want 3s", got)
	}
	if got := parseRetryAfter("0.5", now, 10*time.Second); got != 500*time.Millisecond {
		t.Errorf("float seconds: got %v, want 500ms", got)
	}
	if got := parseRetryAfter("60", now, 5*time.Second); got != 5*time.Second {
		t.Errorf("capped: got %v, want 5s", got)
	}
	future := now.Add(4 * time.Second).UTC().Format(http.TimeFormat)
	if got := parseRetryAfter(future, now, 10*time.Second); got != 4*time.Second {
		t.Errorf("http-date: got %v, want 4s", got)
	}
	if got := parseRetryAfter("-2", now, 10*time.Second); got != 0 {
		t.Errorf("negative: got %v, want 0", got)
	}
	if got := parseRetryAfter("garbage", now, 10*time.Second); got != 0 {
		t.Errorf("garbage: got %v, want 0", got)
	}
}

// 429 属于限流，必须重试（区别于 400/401/404 立即放弃），并遵守 Retry-After。
func TestRunnerRetriesHTTP429WithRetryAfter(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) <= 2 {
			w.Header().Set("Retry-After", "0.04")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(Options{
		MaxConcurrency: 1,
		TimeoutSec:     5,
		MaxAttempts:    3,
		BackoffBaseMs:  5,
		BackoffMaxMs:   500,
	})
	r := NewRunner(c)

	start := time.Now()
	var attempts int
	var lastRes Result
	r.Run(context.Background(), []Task{{
		Adapter: fakeAdapter{}, BaseURL: srv.URL, TransportMode: "non_stream",
	}}, func(idx, attempt int, res Result) {
		attempts++
		lastRes = res
	})
	elapsed := time.Since(start)

	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3 (two 429s + one 200)", attempts)
	}
	if lastRes.ErrorKind != "" {
		t.Fatalf("final attempt failed: %+v", lastRes)
	}
	if elapsed < 70*time.Millisecond {
		t.Errorf("Retry-After 40ms × 2 未生效: elapsed = %v", elapsed)
	}
}

// AIMD 慢启动上探：无错误时应从初始低并发自动探测抬升至 MaxConcurrency。
func TestAIMDProbesUpToCeiling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(15 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(Options{
		MaxConcurrency:     8,
		AutoConcurrency:    true,
		InitialConcurrency: 2,
		BackoffDebounceMs:  20,
		TimeoutSec:         5,
		MaxAttempts:        1,
	})
	r := NewRunner(c)

	var mu sync.Mutex
	var events []ConcurrencyEvent
	r.OnConcurrency = func(ev ConcurrencyEvent) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	}

	tasks := make([]Task, 32)
	for i := range tasks {
		tasks[i] = Task{Adapter: fakeAdapter{}, BaseURL: srv.URL, TransportMode: "non_stream"}
	}
	r.Run(context.Background(), tasks, func(int, int, Result) {})

	snap := r.Concurrency()
	if snap.PeakCwnd < 6 {
		t.Errorf("AIMD 未有效上探并发：PeakCwnd = %d, want >= 6 (events=%+v)", snap.PeakCwnd, events)
	}
	if snap.ThrottleEvents != 0 {
		t.Errorf("无错误场景不应触发退避：ThrottleEvents = %d", snap.ThrottleEvents)
	}
}

// AIMD 遇限流动态回退再恢复：模拟端点真实并发上限为 3（超过 3 返回 429），
// 控制器应从 2 上探触发 429 后乘性回退，且靠重试最终全部成功完成。
func TestAIMDBacksOffOn429AndCompletesAllTasks(t *testing.T) {
	var active atomic.Int32
	var total429 atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := active.Add(1)
		defer active.Add(-1)
		if cur > 3 {
			total429.Add(1)
			w.Header().Set("Retry-After", "0.02")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		time.Sleep(20 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(Options{
		MaxConcurrency:     10,
		AutoConcurrency:    true,
		InitialConcurrency: 2,
		BackoffDebounceMs:  25,
		TimeoutSec:         5,
		MaxAttempts:        5,
		BackoffBaseMs:      10,
		BackoffMaxMs:       100,
	})
	r := NewRunner(c)

	tasks := make([]Task, 30)
	for i := range tasks {
		tasks[i] = Task{Adapter: fakeAdapter{}, BaseURL: srv.URL, TransportMode: "non_stream"}
	}

	var succeeded atomic.Int32
	r.Run(context.Background(), tasks, func(idx, attempt int, res Result) {
		if res.ErrorKind == "" {
			succeeded.Add(1)
		}
	})

	if got := int(succeeded.Load()); got != len(tasks) {
		t.Errorf("期望全部 %d 个任务最终成功，实际成功 %d", len(tasks), got)
	}
	snap := r.Concurrency()
	if total429.Load() > 0 && snap.ThrottleEvents == 0 {
		t.Errorf("触发了 %d 次 429 但 ThrottleEvents=0", total429.Load())
	}
	if snap.CurrentCwnd >= 10 {
		t.Errorf("端点限制并发<=3 时窗口不应停留在上限 10：got %d", snap.CurrentCwnd)
	}
}
