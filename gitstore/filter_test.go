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

// TestBinaryFuseFilterHasNoFalseNegatives pins the pack-tier invariant: every
// inserted key must probe present, since a single miss makes an existing object
// unreachable.
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

// TestFilterProbePositionsUseTheObjectIDBytes pins the claim that the filter
// never re-hashes an object id on the common path.
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
