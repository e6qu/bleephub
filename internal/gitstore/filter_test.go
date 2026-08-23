package gitstore

import (
	"crypto/sha256"
	"encoding/binary"
	"math/rand/v2"
	"testing"
)

// deterministicOIDs produces object ids the way git does — as the digests of
// distinct contents — so the filters are exercised on exactly the input
// distribution they see in production.
func deterministicOIDs(n int) []oidKey {
	keys := make([]oidKey, n)
	var buf [8]byte
	for i := range n {
		binary.LittleEndian.PutUint64(buf[:], uint64(i))
		keys[i] = oidKey(sha256.Sum256(buf[:]))
	}
	return keys
}

func absentOIDs(n int) []oidKey {
	keys := make([]oidKey, n)
	var buf [16]byte
	for i := range n {
		binary.LittleEndian.PutUint64(buf[:], uint64(i))
		binary.LittleEndian.PutUint64(buf[8:], 0xdeadbeefcafe)
		keys[i] = oidKey(sha256.Sum256(buf[:]))
	}
	return keys
}

// TestBinaryFuseFilterHasNoFalseNegatives is the filter invariant for the pack
// tier. Every inserted key must probe as present; a single miss would let an
// object that exists become unreachable.
func TestBinaryFuseFilterHasNoFalseNegatives(t *testing.T) {
	for _, size := range []int{0, 1, 2, 3, 10, 1000, 100000} {
		keys := deterministicOIDs(size)
		filter, err := newBinaryFuseFilter(keys)
		if err != nil {
			t.Fatalf("construct %d keys: %v", size, err)
		}
		for i, key := range keys {
			if !filter.contains(key) {
				t.Fatalf("binary fuse filter of %d keys reported key %d absent", size, i)
			}
		}
	}
}

// TestBinaryFuseFilterFalsePositiveRate pins the space/accuracy point the
// design argument rests on: about eight bits of fingerprint, so roughly one
// false positive in 256, at close to nine bits of storage per key.
func TestBinaryFuseFilterFalsePositiveRate(t *testing.T) {
	const size = 200000
	filter, err := newBinaryFuseFilter(deterministicOIDs(size))
	if err != nil {
		t.Fatalf("construct: %v", err)
	}

	probes := absentOIDs(size)
	positives := 0
	for _, key := range probes {
		if filter.contains(key) {
			positives++
		}
	}
	rate := float64(positives) / float64(len(probes))
	if rate > 0.01 {
		t.Fatalf("false positive rate %.4f, want about 1/256", rate)
	}

	bitsPerKey := float64(filter.bits()) / float64(size)
	if bitsPerKey > 10 {
		t.Fatalf("binary fuse filter used %.2f bits per key, want under 10", bitsPerKey)
	}
	t.Logf("binary fuse: %.4f false positive rate, %.3f bits/key, %d bytes for %d keys",
		rate, bitsPerKey, filter.bits()/8, size)
}

func TestBinaryFuseFilterRoundTrips(t *testing.T) {
	keys := deterministicOIDs(5000)
	filter, err := newBinaryFuseFilter(keys)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	decoded, err := decodeBinaryFuseFilter(filter.encode())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	for i, key := range keys {
		if !decoded.contains(key) {
			t.Fatalf("decoded filter reported key %d absent", i)
		}
	}
	for _, key := range absentOIDs(5000) {
		if decoded.contains(key) != filter.contains(key) {
			t.Fatal("decoded filter disagrees with the original")
		}
	}
	if _, err := decodeBinaryFuseFilter([]byte("short")); err == nil {
		t.Fatal("decoding a truncated filter succeeded")
	}
}

// TestCuckooFilterHasNoFalseNegatives is the filter invariant for the loose
// tier, including after the deletions compaction performs.
func TestCuckooFilterHasNoFalseNegatives(t *testing.T) {
	keys := deterministicOIDs(50000)
	filter := newCuckooFilter(len(keys))
	for _, key := range keys {
		filter.insert(key)
	}
	for i, key := range keys {
		if !filter.contains(key) {
			t.Fatalf("cuckoo filter reported inserted key %d absent", i)
		}
	}

	// Retiring the first half, as compaction does, must not disturb the second.
	for _, key := range keys[:len(keys)/2] {
		filter.remove(key)
	}
	for i, key := range keys[len(keys)/2:] {
		if !filter.contains(key) {
			t.Fatalf("cuckoo filter reported surviving key %d absent after deletions", i)
		}
	}
}

// cuckooResidentBits reports the memory a cuckoo table occupies. It is a
// property of the table's shape rather than of what is in it — an empty slot
// costs the same as a full one — so it belongs with the measurement rather than
// with the structure.
func cuckooResidentBits(c *cuckooFilter) int {
	return len(c.buckets) * cuckooSlotsPerBucket * 16
}

func TestCuckooFilterFalsePositiveRate(t *testing.T) {
	const size = 100000
	filter := newCuckooFilter(size)
	for _, key := range deterministicOIDs(size) {
		filter.insert(key)
	}
	positives := 0
	probes := absentOIDs(size)
	for _, key := range probes {
		if filter.contains(key) {
			positives++
		}
	}
	rate := float64(positives) / float64(len(probes))
	if rate > 0.02 {
		t.Fatalf("cuckoo false positive rate %.4f, want about 1/512", rate)
	}
	t.Logf("cuckoo: %.4f false positive rate, %.3f bits/key, %d bytes for %d keys",
		rate, float64(cuckooResidentBits(filter))/float64(size), cuckooResidentBits(filter)/8, size)
}

// TestCuckooFilterSaturatesRatherThanLosingKeys drives a table far past its
// capacity. A cuckoo table cannot be resized without the keys it was built
// from, so the only two possible behaviours are dropping a key — which would
// make an object that exists answer "absent" and break the filter invariant —
// or refusing to answer negatively at all. This pins the second.
func TestCuckooFilterSaturatesRatherThanLosingKeys(t *testing.T) {
	filter := newCuckooFilter(8)
	keys := deterministicOIDs(20000)
	for _, key := range keys {
		filter.insert(key)
	}
	if !filter.saturated {
		t.Fatal("a table filled to 2500 times its capacity did not report saturation")
	}
	for i, key := range keys {
		if !filter.contains(key) {
			t.Fatalf("key %d answered absent by a saturated filter", i)
		}
	}
	// A saturated filter must not be able to prove anything absent, including
	// keys that were never inserted.
	for _, key := range absentOIDs(100) {
		if !filter.contains(key) {
			t.Fatal("a saturated filter gave a negative answer")
		}
	}
}

// TestCuckooFilterFillsToItsRatedCapacity pins that the ordinary path — a
// filter sized from a directory listing — does not saturate, so the negative
// answers the read path depends on are actually available.
func TestCuckooFilterFillsToItsRatedCapacity(t *testing.T) {
	const size = 100000
	filter := newCuckooFilter(size)
	for _, key := range deterministicOIDs(size) {
		filter.insert(key)
	}
	if filter.saturated {
		t.Fatal("a filter built for its own key count saturated")
	}
}

// TestFilterProbePositionsUseTheObjectIDBytes pins the claim that neither
// structure re-hashes an object id on the common path.
func TestFilterProbePositionsUseTheObjectIDBytes(t *testing.T) {
	var key oidKey
	source := rand.NewChaCha8([32]byte{7})
	_, _ = source.Read(key[:])

	if got := key.positionWord(); got != binary.LittleEndian.Uint64(key[0:8]) {
		t.Fatalf("position word %#x is not the object id's own leading bytes", got)
	}
	if got := key.fingerprintWord(); got != binary.LittleEndian.Uint64(key[8:16]) {
		t.Fatalf("fingerprint word %#x is not read straight from the object id", got)
	}
	if mixSeed(key.positionWord(), 0) != key.positionWord() {
		t.Fatal("the first construction attempt remixed the object id instead of using its bytes")
	}
}
