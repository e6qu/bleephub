package gitstore

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/e6qu/bleephub/gitstore/s3fake"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/storage/memory"
)

// The benchmarks here are the measurement the design is argued from. They run the two tiers — a repository whose objects
// are all loose, and the same repository packed — against one in-process object store that counts every request and
// byte, both built in the same binary from the same code, so the numbers compare.
//
// Each reports three metrics beside per-operation time: the object store requests one clone/push costs, the same per
// object, and bytes transferred. Request count is the quantity of interest: the cost removed is a network round trip, a fixed toll no local speed pays off.
//
// Environment:
//
//	GITSTORE_BENCH_OBJECTS  objects to seed (default 1000)
//	GITSTORE_BENCH_LATENCY  delay applied to every request, standing
//	                                 in for the round trip to a real endpoint
//	                                 (default 0, which measures CPU only)

const benchRepo = "octocat/monorepo"

func benchObjects(tb testing.TB) int {
	tb.Helper()
	raw := strings.TrimSpace(os.Getenv("GITSTORE_BENCH_OBJECTS"))
	if raw == "" {
		return 1000
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		tb.Fatalf("GITSTORE_BENCH_OBJECTS=%q is not a positive count", raw)
	}
	return n
}

func benchLatency(tb testing.TB) time.Duration {
	tb.Helper()
	raw := strings.TrimSpace(os.Getenv("GITSTORE_BENCH_LATENCY"))
	if raw == "" {
		return 0
	}
	latency, err := time.ParseDuration(raw)
	if err != nil || latency < 0 {
		tb.Fatalf("GITSTORE_BENCH_LATENCY=%q is not a non-negative duration", raw)
	}
	return latency
}

// report attaches the measured request and byte counts to the benchmark result.
func report(b *testing.B, counts s3fake.Counts, objects int) {
	b.ReportMetric(float64(counts.Total()), "s3-requests/op")
	b.ReportMetric(float64(counts.Total())/float64(objects), "s3-requests/object")
	b.ReportMetric(float64(counts.BytesDown+counts.BytesUp), "bytes/op")
	b.Logf("objects=%d %s", objects, counts)
}

// newBenchFakeS3 turns off the automatic compaction trigger, so a background compaction cannot fire mid-phase and be
// charged to it. The pack cache is already a directory of this benchmark's own.
func newBenchFakeS3(b *testing.B) *fakeS3 {
	b.Helper()
	fake := newFakeS3(b)
	fake.opts.CompactionTrigger = -1
	return fake
}

// BenchmarkPushLoose measures writing objects one at a time into the loose tier, which is what the REST git-database
// endpoints and web edits cost. A push does not take this path: see BenchmarkPushPack.
func BenchmarkPushLoose(b *testing.B) {
	objects := benchObjects(b)
	var counts s3fake.Counts
	seeded := 1
	for b.Loop() {
		b.StopTimer()
		fake := newBenchFakeS3(b)
		stor, err := packedStorage(fake, benchRepo)
		if err != nil {
			b.Fatalf("storage: %v", err)
		}
		fake.SetLatency(benchLatency(b))
		b.StartTimer()

		hashes := seedObjects(b, stor, objects)

		b.StopTimer()
		fake.SetLatency(0)
		counts = fake.Snapshot()
		seeded = len(hashes)
		b.StartTimer()
	}
	report(b, counts, seeded)
}

// BenchmarkPushPack measures ingesting the same objects as the packfile a push sends, which is published as a pack: a
// fixed handful of requests, however many objects it carries.
func BenchmarkPushPack(b *testing.B) {
	objects := benchObjects(b)
	client := memory.NewStorage()
	hashes := seedObjects(b, client, objects)
	var pack bytes.Buffer
	if _, err := packfile.NewEncoder(&pack, client, false).Encode(hashes, gitPackWindow); err != nil {
		b.Fatalf("encode push: %v", err)
	}

	var counts s3fake.Counts
	for b.Loop() {
		b.StopTimer()
		fake := newBenchFakeS3(b)
		stor, err := packedStorage(fake, benchRepo)
		if err != nil {
			b.Fatalf("storage: %v", err)
		}
		fake.SetLatency(benchLatency(b))
		b.StartTimer()

		if err := packfile.UpdateObjectStorage(stor, bytes.NewReader(pack.Bytes())); err != nil {
			b.Fatalf("ingest push: %v", err)
		}

		b.StopTimer()
		fake.SetLatency(0)
		counts = fake.Snapshot()
		b.StartTimer()
	}
	report(b, counts, len(hashes))
}

// BenchmarkCloneLoose is the baseline: a clone served entirely out of the loose tier, where every object is one whole-object GET.
func BenchmarkCloneLoose(b *testing.B) {
	fake := newBenchFakeS3(b)
	stor, err := packedStorage(fake, benchRepo)
	if err != nil {
		b.Fatalf("storage: %v", err)
	}
	hashes := seedObjects(b, stor, benchObjects(b))

	var counts s3fake.Counts
	for b.Loop() {
		b.StopTimer()
		// A clone is served by a storer opened for the request, so the writing handle's warm in-process object cache must not be counted as a saving the read path actually has.
		readStor, err := packedStorage(fake, benchRepo)
		if err != nil {
			b.Fatalf("storage: %v", err)
		}
		before := fake.Snapshot()
		fake.SetLatency(benchLatency(b))
		b.StartTimer()

		clonePack(b, readStor, hashes)

		b.StopTimer()
		fake.SetLatency(0)
		counts = fake.Snapshot().Sub(before)
		b.StartTimer()
	}
	report(b, counts, len(hashes))
}

// BenchmarkCompaction measures moving a repository's loose tier into a pack. It is the cost the read path's saving is bought with, paid once per batch of objects rather than once per clone.
func BenchmarkCompaction(b *testing.B) {
	objects := benchObjects(b)
	var counts s3fake.Counts
	packed := 1
	for b.Loop() {
		b.StopTimer()
		fake := newBenchFakeS3(b)
		stor, err := packedStorage(fake, benchRepo)
		if err != nil {
			b.Fatalf("storage: %v", err)
		}
		hashes := seedObjects(b, stor, objects)
		before := fake.Snapshot()
		fake.SetLatency(benchLatency(b))
		b.StartTimer()

		result, err := CompactRepository(context.Background(), stor)

		b.StopTimer()
		fake.SetLatency(0)
		if err != nil {
			b.Fatalf("compact: %v", err)
		}
		if result.Packed != len(hashes) {
			b.Fatalf("packed %d of %d objects", result.Packed, len(hashes))
		}
		counts = fake.Snapshot().Sub(before)
		packed = len(hashes)
		b.StartTimer()
	}
	report(b, counts, packed)
}

// BenchmarkClonePackedColdCache is the number the design exists to move: a clone of a packed repository served by a
// replica that has never seen it, so every byte comes from the object store through ranged reads.
func BenchmarkClonePackedColdCache(b *testing.B) {
	fake, hashes := benchPackedRepository(b)

	var counts s3fake.Counts
	for b.Loop() {
		b.StopTimer()
		clearPackCache(b, fake.opts.CacheDir)
		readStor, err := packedStorage(fake, benchRepo)
		if err != nil {
			b.Fatalf("storage: %v", err)
		}
		before := fake.Snapshot()
		fake.SetLatency(benchLatency(b))
		b.StartTimer()

		clonePack(b, readStor, hashes)

		b.StopTimer()
		fake.SetLatency(0)
		counts = fake.Snapshot().Sub(before)
		b.StartTimer()
	}
	report(b, counts, len(hashes))
}

// BenchmarkClonePackedWarmCache is the steady state: a replica serving a repository whose pack it already holds locally.
func BenchmarkClonePackedWarmCache(b *testing.B) {
	fake, hashes := benchPackedRepository(b)
	warm, err := packedStorage(fake, benchRepo)
	if err != nil {
		b.Fatalf("storage: %v", err)
	}
	clonePack(b, warm, hashes)

	var counts s3fake.Counts
	for b.Loop() {
		b.StopTimer()
		readStor, err := packedStorage(fake, benchRepo)
		if err != nil {
			b.Fatalf("storage: %v", err)
		}
		before := fake.Snapshot()
		fake.SetLatency(benchLatency(b))
		b.StartTimer()

		clonePack(b, readStor, hashes)

		b.StopTimer()
		fake.SetLatency(0)
		counts = fake.Snapshot().Sub(before)
		b.StartTimer()
	}
	report(b, counts, len(hashes))
}

// BenchmarkHasEncodedObjectAbsent measures the question a fetch negotiation asks about objects the repository does not have.
// Before the membership index each one was an object store GET that returned a 404.
func BenchmarkHasEncodedObjectAbsent(b *testing.B) {
	fake, _ := benchPackedRepository(b)
	stor, err := packedStorage(fake, benchRepo)
	if err != nil {
		b.Fatalf("storage: %v", err)
	}
	if err := stor.HasEncodedObject(absentHash(0)); err == nil {
		b.Fatal("an object that was never written was reported present")
	}

	fake.SetLatency(benchLatency(b))
	before := fake.Snapshot()
	probes := 0
	for b.Loop() {
		probes++
		if err := stor.HasEncodedObject(absentHash(probes)); err == nil {
			b.Fatal("an absent object was reported present")
		}
	}
	b.StopTimer()
	fake.SetLatency(0)
	report(b, fake.Snapshot().Sub(before), max(probes, 1))
}

// benchPackedRepository seeds and compacts a repository, returning the store it lives in and every object in it.
func benchPackedRepository(b *testing.B) (*fakeS3, []plumbing.Hash) {
	b.Helper()
	fake := newBenchFakeS3(b)
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
