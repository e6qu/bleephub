package gitstore

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
)

// The benchmarks here are the measurement the design is argued from. They run the two storage shapes — loose tier alone
// (what this package had) and loose tier with the pack tier under it (what it has now) — against one in-process object
// store that counts every request and byte, both built in the same binary from the same code, so the numbers compare.
//
// Each reports three metrics beside per-operation time: the object store requests one clone/push costs, the same per
// object, and bytes transferred. Request count is the quantity of interest: the cost removed is a network round trip, a fixed toll no local speed pays off.
//
// Environment:
//
//	BLEEPHUB_GITSTORE_BENCH_OBJECTS  objects to seed (default 1000)
//	BLEEPHUB_GITSTORE_BENCH_LATENCY  delay applied to every request, standing
//	                                 in for the round trip to a real endpoint
//	                                 (default 0, which measures CPU only)

const benchRepo = "octocat/monorepo"

func benchObjects(tb testing.TB) int {
	tb.Helper()
	raw := strings.TrimSpace(os.Getenv("BLEEPHUB_GITSTORE_BENCH_OBJECTS"))
	if raw == "" {
		return 1000
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		tb.Fatalf("BLEEPHUB_GITSTORE_BENCH_OBJECTS=%q is not a positive count", raw)
	}
	return n
}

func benchLatency() time.Duration {
	return envDuration("BLEEPHUB_GITSTORE_BENCH_LATENCY", 0)
}

// report attaches the measured request and byte counts to the benchmark result.
func report(b *testing.B, counts s3Counts, objects int) {
	b.ReportMetric(float64(counts.total()), "s3-requests/op")
	b.ReportMetric(float64(counts.total())/float64(objects), "s3-requests/object")
	b.ReportMetric(float64(counts.bytesDown+counts.bytesUp), "bytes/op")
	b.Logf("objects=%d %s", objects, counts)
}

// benchSetup turns off the automatic compaction trigger, so a background compaction cannot fire mid-phase and be charged to it,
// and points the pack cache at a directory of this benchmark's own.
func benchSetup(b *testing.B) {
	b.Helper()
	b.Setenv(compactionTriggerEnv, "0")
	b.Setenv(packCacheDirEnv, b.TempDir())
}

// BenchmarkPushLoose measures writing objects into the loose tier, which is what a push costs and is unchanged by the pack tier: an object still arrives as one key.
func BenchmarkPushLoose(b *testing.B) {
	benchSetup(b)
	objects := benchObjects(b)
	var counts s3Counts
	seeded := 1
	for b.Loop() {
		b.StopTimer()
		fake := newFakeS3(b)
		stor, err := packedStorage(fake, benchRepo)
		if err != nil {
			b.Fatalf("storage: %v", err)
		}
		fake.setLatency(benchLatency())
		b.StartTimer()

		hashes := seedObjects(b, stor, objects)

		b.StopTimer()
		fake.setLatency(0)
		counts = fake.snapshot()
		seeded = len(hashes)
		b.StartTimer()
	}
	report(b, counts, seeded)
}

// BenchmarkCloneLoose is the baseline: a clone served entirely out of the loose tier, where every object is one whole-object GET.
func BenchmarkCloneLoose(b *testing.B) {
	benchSetup(b)
	fake := newFakeS3(b)
	stor, err := looseStorage(fake, benchRepo)
	if err != nil {
		b.Fatalf("storage: %v", err)
	}
	hashes := seedObjects(b, stor, benchObjects(b))

	var counts s3Counts
	for b.Loop() {
		b.StopTimer()
		// A clone is served by a storer opened for the request, so the writing handle's warm in-process object cache must not be counted as a saving the read path actually has.
		readStor, err := looseStorage(fake, benchRepo)
		if err != nil {
			b.Fatalf("storage: %v", err)
		}
		before := fake.snapshot()
		fake.setLatency(benchLatency())
		b.StartTimer()

		clonePack(b, readStor, hashes)

		b.StopTimer()
		fake.setLatency(0)
		counts = fake.snapshot().sub(before)
		b.StartTimer()
	}
	report(b, counts, len(hashes))
}

// BenchmarkCompaction measures moving a repository's loose tier into a pack. It is the cost the read path's saving is bought with, paid once per batch of objects rather than once per clone.
func BenchmarkCompaction(b *testing.B) {
	benchSetup(b)
	objects := benchObjects(b)
	var counts s3Counts
	packed := 1
	for b.Loop() {
		b.StopTimer()
		fake := newFakeS3(b)
		stor, err := packedStorage(fake, benchRepo)
		if err != nil {
			b.Fatalf("storage: %v", err)
		}
		hashes := seedObjects(b, stor, objects)
		before := fake.snapshot()
		fake.setLatency(benchLatency())
		b.StartTimer()

		result, err := CompactRepository(context.Background(), stor)

		b.StopTimer()
		fake.setLatency(0)
		if err != nil {
			b.Fatalf("compact: %v", err)
		}
		if result.Packed != len(hashes) {
			b.Fatalf("packed %d of %d objects", result.Packed, len(hashes))
		}
		counts = fake.snapshot().sub(before)
		packed = len(hashes)
		b.StartTimer()
	}
	report(b, counts, packed)
}

// BenchmarkClonePackedColdCache is the number the design exists to move: a clone of a packed repository served by a
// replica that has never seen it, so every byte comes from the object store through ranged reads.
func BenchmarkClonePackedColdCache(b *testing.B) {
	benchSetup(b)
	fake, hashes := benchPackedRepository(b)

	var counts s3Counts
	for b.Loop() {
		b.StopTimer()
		clearPackCache(b, os.Getenv(packCacheDirEnv))
		readStor, err := packedStorage(fake, benchRepo)
		if err != nil {
			b.Fatalf("storage: %v", err)
		}
		before := fake.snapshot()
		fake.setLatency(benchLatency())
		b.StartTimer()

		clonePack(b, readStor, hashes)

		b.StopTimer()
		fake.setLatency(0)
		counts = fake.snapshot().sub(before)
		b.StartTimer()
	}
	report(b, counts, len(hashes))
}

// BenchmarkClonePackedWarmCache is the steady state: a replica serving a repository whose pack it already holds locally.
func BenchmarkClonePackedWarmCache(b *testing.B) {
	benchSetup(b)
	fake, hashes := benchPackedRepository(b)
	warm, err := packedStorage(fake, benchRepo)
	if err != nil {
		b.Fatalf("storage: %v", err)
	}
	clonePack(b, warm, hashes)

	var counts s3Counts
	for b.Loop() {
		b.StopTimer()
		readStor, err := packedStorage(fake, benchRepo)
		if err != nil {
			b.Fatalf("storage: %v", err)
		}
		before := fake.snapshot()
		fake.setLatency(benchLatency())
		b.StartTimer()

		clonePack(b, readStor, hashes)

		b.StopTimer()
		fake.setLatency(0)
		counts = fake.snapshot().sub(before)
		b.StartTimer()
	}
	report(b, counts, len(hashes))
}

// BenchmarkHasEncodedObjectAbsent measures the question a fetch negotiation asks about objects the repository does not have.
// Before the membership index each one was an object store GET that returned a 404.
func BenchmarkHasEncodedObjectAbsent(b *testing.B) {
	benchSetup(b)
	fake, _ := benchPackedRepository(b)
	stor, err := packedStorage(fake, benchRepo)
	if err != nil {
		b.Fatalf("storage: %v", err)
	}
	if err := stor.HasEncodedObject(absentHash(0)); err == nil {
		b.Fatal("an object that was never written was reported present")
	}

	fake.setLatency(benchLatency())
	before := fake.snapshot()
	probes := 0
	for b.Loop() {
		probes++
		if err := stor.HasEncodedObject(absentHash(probes)); err == nil {
			b.Fatal("an absent object was reported present")
		}
	}
	b.StopTimer()
	fake.setLatency(0)
	report(b, fake.snapshot().sub(before), max(probes, 1))
}

// benchPackedRepository seeds and compacts a repository, returning the store it lives in and every object in it.
func benchPackedRepository(b *testing.B) (*fakeS3, []plumbing.Hash) {
	b.Helper()
	fake := newFakeS3(b)
	stor, err := packedStorage(fake, benchRepo)
	if err != nil {
		b.Fatalf("storage: %v", err)
	}
	hashes := seedObjects(b, stor, benchObjects(b))
	result, err := CompactRepository(context.Background(), stor)
	if err != nil {
		b.Fatalf("compact: %v", err)
	}
	if result.Packed != len(hashes) {
		b.Fatalf("packed %d of %d objects", result.Packed, len(hashes))
	}
	return fake, hashes
}

// clearPackCache empties the local pack cache so a measurement starts from the state of a replica that has never served this repository.
func clearPackCache(tb testing.TB, dir string) {
	tb.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		tb.Fatalf("read cache dir: %v", err)
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			tb.Fatalf("clear cache: %v", err)
		}
	}
	packCaches.Delete(dir)
}
