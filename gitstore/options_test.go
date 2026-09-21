package gitstore

import (
	"testing"
	"time"
)

func TestZeroOptionsSelectEveryDefault(t *testing.T) {
	got := Options{}.resolved()
	if got.Region != "us-east-1" || got.ChunkBytes != 4<<20 || got.CacheBytes != 8<<30 ||
		got.MemoryCacheBytes != 256<<20 || got.IndexFreshness != 250*time.Millisecond ||
		got.CompactAfterPacks != 8 || got.MultipartBytes != 64<<20 ||
		got.BreakerThreshold != 5 || got.BreakerCooldown != 5*time.Second || got.CacheDir == "" {
		t.Fatalf("resolved defaults = %+v", got)
	}
}

// TestNegativeOptionsSelectOff pins the sentinel: for a tunable with a
// meaningful "off", zero is already taken by "use the default", so negative
// must resolve to the zero the consuming code treats as off.
func TestNegativeOptionsSelectOff(t *testing.T) {
	got := Options{MemoryCacheBytes: -1, IndexFreshness: -1, CompactAfterPacks: -1, BreakerThreshold: -1}.resolved()
	if got.MemoryCacheBytes != 0 || got.IndexFreshness != 0 || got.CompactAfterPacks != 0 || got.BreakerThreshold != 0 {
		t.Fatalf("negative options did not resolve to off: %+v", got)
	}
}

func TestExplicitOptionsSurviveResolution(t *testing.T) {
	want := Options{
		Region: "eu-west-1", ChunkBytes: 4096, CacheDir: "/cache", CacheBytes: 1 << 20, MemoryCacheBytes: 1 << 10,
		IndexFreshness: time.Hour, CompactAfterPacks: 7, MultipartBytes: 1024, BreakerThreshold: 2, BreakerCooldown: time.Minute,
	}
	if got := want.resolved(); got != want {
		t.Fatalf("resolved = %+v, want %+v", got, want)
	}
}
