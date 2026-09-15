package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/starcat-app/starcat-history-api/internal/telemetry"
)

// 全局闸门必须真的限住并发：这是"多个仓库同时冷启动"时唯一能防住 GitHub
// 二级限流的地方。
func TestConcurrencyLimitCapsInFlightRequests(t *testing.T) {
	const limit = 2
	var inFlight atomic.Int64
	var peak atomic.Int64
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			previous := peak.Load()
			if current <= previous || peak.CompareAndSwap(previous, current) {
				break
			}
		}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(repoPayload))
	}))
	defer server.Close()

	registry := telemetry.NewRegistry()
	provider := NewGitHubProvider(server.URL, "", server.Client()).
		WithTelemetry(registry).
		WithConcurrencyLimit(limit)
	// 闸门饱和时上层要等排队的请求退场，这里把等待压到很短以免测试卡住。
	provider.limiter.acquireTimeout = 200 * time.Millisecond

	var waitGroup sync.WaitGroup
	for i := 0; i < limit; i++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			_, _ = provider.Fetch(t.Context(), "o", "r")
		}()
	}
	// 等两个请求都进入服务端，再放行。
	deadline := time.Now().Add(2 * time.Second)
	for inFlight.Load() < limit && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	waitGroup.Wait()

	if got := peak.Load(); got > limit {
		t.Fatalf("in-flight peak %d exceeded the configured limit %d", got, limit)
	}
}

// 排队超时必须返回 ErrBusy 并计数，而不是无限等待：
// 上层靠这个错误回退到缓存或 stale 数据。
func TestLimiterSaturationReturnsBusy(t *testing.T) {
	blocked := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-blocked
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(repoPayload))
	}))
	defer server.Close()

	registry := telemetry.NewRegistry()
	provider := NewGitHubProvider(server.URL, "", server.Client()).
		WithTelemetry(registry).
		WithConcurrencyLimit(1)
	provider.limiter.acquireTimeout = 100 * time.Millisecond

	var waitGroup sync.WaitGroup
	waitGroup.Add(1)
	go func() {
		defer waitGroup.Done()
		_, _ = provider.Fetch(t.Context(), "o", "r")
	}()
	// 占满唯一槽位后再发一个：应快速失败为 ErrBusy。
	deadline := time.Now().Add(2 * time.Second)
	for len(provider.limiter.slots) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	_, err := provider.Fetch(t.Context(), "o", "r")
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("expected ErrBusy when the limiter is saturated, got %v", err)
	}
	if got := registry.Snapshot().LimiterTimeouts; got != 1 {
		t.Fatalf("expected one limiter timeout, got %d", got)
	}
	close(blocked)
	waitGroup.Wait()
}

// 未装配闸门时行为不变：单测与本地调试不该被迫配并发上限。
func TestNoLimiterMeansUnbounded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(repoPayload))
	}))
	defer server.Close()
	provider := NewGitHubProvider(server.URL, "", server.Client())
	if provider.limiter != nil {
		t.Fatal("limiter must stay nil unless configured")
	}
	if _, err := provider.Fetch(context.Background(), "o", "r"); err != nil {
		t.Fatal(err)
	}
}
