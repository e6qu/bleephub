package store

import (
	"context"
	"strconv"
	"sync"
	"testing"
)

// BenchmarkDurableWriteThroughput compares end-to-end durable-write throughput
// (each writer commits then waits for durability) with group commit off vs on,
// under concurrency. Off, every write fsyncs synchronously and serializes on the
// persistence lock; on, the committer batches many writers' ops into one fsync.
//
//	go test -tags noui -run x -bench DurableWriteThroughput ./internal/store/
func BenchmarkDurableWriteThroughput(b *testing.B) {
	for _, on := range []bool{false, true} {
		name := "sync"
		if on {
			name = "groupcommit"
		}
		b.Run(name, func(b *testing.B) {
			dir := b.TempDir()
			b.Setenv("BLEEPHUB_PERSIST", "true")
			b.Setenv("BLEEPHUB_DATA_DIR", dir)
			b.Setenv("BLEEPHUB_PERSISTENCE_ENCRYPTION_KEY", testEncryptionKey)
			if on {
				b.Setenv("BLEEPHUB_GROUP_COMMIT", "true")
			} else {
				b.Setenv("BLEEPHUB_GROUP_COMMIT", "")
			}
			p, err := NewPersistence()
			if err != nil {
				b.Fatalf("open: %v", err)
			}
			defer p.Close()

			const writers = 32
			var counter int64
			var mu sync.Mutex
			next := func() int64 { mu.Lock(); defer mu.Unlock(); counter++; return counter }

			b.ResetTimer()
			var wg sync.WaitGroup
			perWriter := b.N / writers
			if perWriter == 0 {
				perWriter = 1
			}
			for w := 0; w < writers; w++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := 0; i < perWriter; i++ {
						k := strconv.FormatInt(next(), 10)
						if err := p.PutBatch(PersistencePut{Bucket: "kv-bench", Key: k, Value: k}); err != nil {
							b.Errorf("put: %v", err)
							return
						}
						// End-to-end durability, as an acked API write would require.
						if err := p.WaitDurable(context.Background(), p.EnqueuedSeq()); err != nil {
							b.Errorf("wait: %v", err)
							return
						}
					}
				}()
			}
			wg.Wait()
		})
	}
}
