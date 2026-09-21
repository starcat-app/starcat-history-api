package telemetry

import "testing"

func TestSnapshotCountsEveryCategoryIndependently(t *testing.T) {
	registry := NewRegistry()
	registry.MetadataRequested()
	registry.MetadataRequested()
	registry.HistoryRequested()
	registry.AvatarRequested()
	registry.RateLimited()
	registry.LimiterTimeout()
	registry.MetadataCacheHit()
	registry.MetadataCacheMiss()
	registry.MetadataNegativeHit()
	registry.HistoryCacheHit()
	registry.HistoryCacheMiss()
	registry.HistoryStaleServed()

	got := registry.Snapshot()
	want := Snapshot{
		GitHubMetadataRequests: 2,
		GitHubHistoryRequests:  1,
		GitHubAvatarRequests:   1,
		GitHubRateLimited:      1,
		LimiterTimeouts:        1,
		MetadataCacheHits:      1,
		MetadataCacheMisses:    1,
		MetadataNegativeHits:   1,
		HistoryCacheHits:       1,
		HistoryCacheMisses:     1,
		HistoryStaleServed:     1,
	}
	if got != want {
		t.Fatalf("unexpected snapshot\n got: %+v\nwant: %+v", got, want)
	}
}

// 压测脚本依赖 Reset 取「单场景差值」，所以它必须把每一类都清零，
// 漏掉一类就会让下一个场景的读数偏高。
func TestResetClearsEveryCounter(t *testing.T) {
	registry := NewRegistry()
	registry.MetadataRequested()
	registry.HistoryRequested()
	registry.AvatarRequested()
	registry.RateLimited()
	registry.LimiterTimeout()
	registry.MetadataCacheHit()
	registry.MetadataCacheMiss()
	registry.MetadataNegativeHit()
	registry.HistoryCacheHit()
	registry.HistoryCacheMiss()
	registry.HistoryStaleServed()

	registry.Reset()
	if got := registry.Snapshot(); got != (Snapshot{}) {
		t.Fatalf("reset left counters behind: %+v", got)
	}
}

// 未装配埋点的调用方（provider / handler 的零值配置）会拿着 nil 调计数方法，
// 这里把 nil-safe 约定固定下来：不能 panic，也不能把业务调用带崩。
func TestNilRegistryIsSafe(t *testing.T) {
	var registry *Registry
	registry.MetadataRequested()
	registry.HistoryRequested()
	registry.AvatarRequested()
	registry.RateLimited()
	registry.LimiterTimeout()
	registry.MetadataCacheHit()
	registry.MetadataCacheMiss()
	registry.MetadataNegativeHit()
	registry.HistoryCacheHit()
	registry.HistoryCacheMiss()
	registry.HistoryStaleServed()
	if got := registry.Snapshot(); got != (Snapshot{}) {
		t.Fatalf("nil registry must report empty snapshot, got %+v", got)
	}
	registry.Reset()
}
