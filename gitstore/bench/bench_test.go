package main

import (
	"bytes"
	"context"
	"net/url"
	"testing"

	"github.com/e6qu/bleephub/gitstore/s3fake"
	minio "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

var tinySpec = WorkloadSpec{Files: 48, Commits: 3, Pushes: 2, Changes: 4, Seed: 7}

// TestWorkloadIsReproducible pins the property every comparison rests on: two
// generations from one seed are the same repository, byte for byte, so two
// drivers — or two runs a month apart — are handed identical pushes.
func TestWorkloadIsReproducible(t *testing.T) {
	first, err := GenerateWorkload(tinySpec)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	second, err := GenerateWorkload(tinySpec)
	if err != nil {
		t.Fatalf("generate again: %v", err)
	}
	if first.Initial.Tip != second.Initial.Tip || !bytes.Equal(first.Initial.Pack, second.Initial.Pack) {
		t.Fatalf("initial push differs between generations: %s vs %s", first.Initial.Tip, second.Initial.Tip)
	}
	if len(first.Incremental) != tinySpec.Pushes || len(second.Incremental) != tinySpec.Pushes {
		t.Fatalf("incremental pushes = %d and %d, want %d", len(first.Incremental), len(second.Incremental), tinySpec.Pushes)
	}
	for i := range first.Incremental {
		if !bytes.Equal(first.Incremental[i].Pack, second.Incremental[i].Pack) {
			t.Fatalf("incremental push %d differs between generations", i)
		}
	}

	other := tinySpec
	other.Seed++
	different, err := GenerateWorkload(other)
	if err != nil {
		t.Fatalf("generate other seed: %v", err)
	}
	if different.Initial.Tip == first.Initial.Tip {
		t.Fatal("a different seed produced the same repository")
	}
}

// TestMeterCountsWhatTheStoreCounts pins the meter against the fake it fronts.
// The meter is what makes drivers comparable, and a driver-side figure quoted
// from it is only meaningful if it files each request under the same operation
// the object store itself would.
func TestMeterCountsWhatTheStoreCounts(t *testing.T) {
	fake := s3fake.New()
	t.Cleanup(fake.Close)
	target, err := url.Parse(fake.URL())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	meter := NewMeter(target)
	t.Cleanup(meter.Close)

	endpoint, err := url.Parse(meter.URL())
	if err != nil {
		t.Fatalf("parse meter: %v", err)
	}
	client, err := minio.NewCore(endpoint.Host, &minio.Options{
		Creds:        credentials.NewStaticV4("fake", "fake", ""),
		Region:       "us-east-1",
		BucketLookup: minio.BucketLookupPath,
		MaxRetries:   1,
	})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	ctx := context.Background()
	body := bytes.Repeat([]byte("x"), 4096)

	if _, err := client.Client.PutObject(ctx, "bucket", "dir/a", bytes.NewReader(body), int64(len(body)), minio.PutObjectOptions{}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, err := client.Client.StatObject(ctx, "bucket", "dir/a", minio.StatObjectOptions{}); err != nil {
		t.Fatalf("head: %v", err)
	}
	ranged := minio.GetObjectOptions{}
	if err := ranged.SetRange(0, 99); err != nil {
		t.Fatalf("range: %v", err)
	}
	for _, opts := range []minio.GetObjectOptions{{}, ranged} {
		reader, _, _, err := client.GetObject(ctx, "bucket", "dir/a", opts)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if _, err := bytes.NewBuffer(nil).ReadFrom(reader); err != nil {
			t.Fatalf("read: %v", err)
		}
		_ = reader.Close()
	}
	if _, _, _, err := client.GetObject(ctx, "bucket", "dir/missing", minio.GetObjectOptions{}); err == nil {
		t.Fatal("reading a missing key succeeded")
	}
	if _, err := client.CopyObject(ctx, "bucket", "dir/a", "bucket", "dir/b", nil, minio.CopySrcOptions{}, minio.PutObjectOptions{}); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if _, err := client.ListObjectsV2("bucket", "dir/", "", "", "", 1000); err != nil {
		t.Fatalf("list: %v", err)
	}
	if err := client.Client.RemoveObject(ctx, "bucket", "dir/b", minio.RemoveObjectOptions{}); err != nil {
		t.Fatalf("delete: %v", err)
	}

	metered, stored := meter.Snapshot(), fake.Snapshot()
	// Operations must agree exactly. Bytes need not: the store counts object
	// payload, while the meter counts the wire — listings, error documents and
	// an upload's chunked-signature framing included — so it can only see more.
	meteredOps, storedOps := metered, stored
	meteredOps.BytesUp, storedOps.BytesUp, meteredOps.BytesDown, storedOps.BytesDown = 0, 0, 0, 0
	if meteredOps != storedOps {
		t.Fatalf("meter and store disagree:\n meter %s\n store %s", metered, stored)
	}
	if metered.BytesUp < stored.BytesUp || metered.BytesDown < stored.BytesDown {
		t.Fatalf("meter saw fewer bytes than the store moved:\n meter %s\n store %s", metered, stored)
	}
	want := s3fake.Counts{Get: 2, GetRanged: 1, Head: 1, Put: 1, Copy: 1, List: 1, Delete: 1, NotFoundGet: 1}
	if storedOps != want {
		t.Fatalf("operations = %s, want %s", stored, want)
	}
}

// TestEveryDriverCompletesEveryScenario runs the whole harness at its smallest.
// It is the guard against a driver rotting: each scenario verifies the objects
// it read back, so a pass means every driver stored and served the repository
// correctly, not merely that nothing panicked.
func TestEveryDriverCompletesEveryScenario(t *testing.T) {
	workload, err := GenerateWorkload(tinySpec)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	fake := s3fake.New()
	t.Cleanup(fake.Close)
	target, err := url.Parse(fake.URL())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	meter := NewMeter(target)
	t.Cleanup(meter.Close)

	selected := map[string]bool{}
	for _, name := range scenarioOrder {
		selected[name] = true
	}
	for _, name := range storerDriverNames() {
		t.Run(name, func(t *testing.T) {
			driver, err := newStorerDriver(name)
			if err != nil {
				t.Fatalf("driver: %v", err)
			}
			env := Env{
				Endpoint: meter.URL(), Bucket: "bucket", Region: "us-east-1",
				AccessKey: "fake", SecretKey: "fake", Prefix: "test-" + name, TempDir: t.TempDir(),
			}
			if err := driver.Setup(context.Background(), env); err != nil {
				t.Fatalf("setup: %v", err)
			}
			t.Cleanup(func() { _ = driver.Close() })

			r := &runner{
				driver: driver, meter: meter, workload: workload, repo: "bench/repo",
				parallel: 2, probes: 16, selected: selected,
			}
			ran := map[string]bool{}
			for _, result := range r.execute(context.Background()) {
				if result.Error != "" {
					t.Errorf("%s: %s", result.Scenario, result.Error)
				}
				ran[result.Scenario] = true
			}
			for _, scenario := range []string{scenarioPushInitial, scenarioCloneCold, scenarioPushIncremental, scenarioFetch, scenarioCloneParallel} {
				if !ran[scenario] {
					t.Errorf("scenario %s did not run", scenario)
				}
			}
		})
	}
}
