package gitstore

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/bits"
)

// Two approximate membership structures for the object index.
//
// FILTER INVARIANT: a filter here may only be believed when it says NO. Both
// are one-sided — "absent" is a proof, "present" a hint — so a "present" answer
// only ever triggers the exact lookup the caller would have done anyway, and no
// filter result decides the outcome of a git operation. A false "absent" is the
// only way a filter can do harm, so the absent answer must be a proof.
//
// Object ids are already uniform cryptographic hashes, so both structures draw
// probe positions straight from the id's bytes rather than re-hashing.

// oidKey is the fixed-width view the filters take of an object id, so neither
// filter has to know whether the hash is 20 bytes (SHA-1) or 32 (SHA-256).
type oidKey [32]byte

func oidKeyFrom(raw []byte) oidKey {
	var key oidKey
	copy(key[:], raw)
	return key
}

// positionWord is the 64 bits that place a key in the table.
func (k oidKey) positionWord() uint64 { return binary.LittleEndian.Uint64(k[0:8]) }

// fingerprintWord uses bytes the position word does not, keeping slot and
// fingerprint independent.
func (k oidKey) fingerprintWord() uint64 { return binary.LittleEndian.Uint64(k[8:16]) }

// ---------------------------------------------------------------------------
// Binary fuse filter, used for the immutable per-pack object sets.
// ---------------------------------------------------------------------------

// binaryFuseFilter is a binary fuse filter with 8-bit fingerprints, used only
// for packs: it is static (the full key set must be known at construction, with
// no add or remove afterward), which is exactly a packfile — written once and
// discarded whole when compaction supersedes it. Chosen over a Bloom filter on
// space (~9 bits/key vs ~11.5 for 2^-8) and cache behavior (three slots in a
// window of three segments vs six scattered words). A ribbon filter saves under
// 5% more, not worth a banded-linear-system query in corruption-critical code.
type binaryFuseFilter struct {
	seed               uint64
	segmentLength      uint32
	segmentLengthMask  uint32
	segmentCount       uint32
	segmentCountLength uint32
	fingerprints       []uint8
}

// binaryFuseMaxAttempts bounds construction retries so a key set that never
// peels becomes a reportable error instead of a non-terminating loop.
const binaryFuseMaxAttempts = 100

var errBinaryFuseConstruction = errors.New("binary fuse filter: construction did not converge")

// binaryFuseMaxKeys bounds the key set: slots are uint32-addressed and the table
// is sized above the key count. Refuse a larger set rather than size from a
// wrapped count, which would place keys where the probe never visits — a false
// "absent" the invariant forbids.
const binaryFuseMaxKeys = math.MaxUint32 / 2

var errBinaryFuseTooManyKeys = errors.New("binary fuse filter: more keys than the table can address")

// mulhiBounded maps a hash uniformly onto [0, bound) via the high half of the
// 128-bit product (multiply-shift, not modulo).
func mulhiBounded(hash uint64, bound uint32) uint32 {
	high, _ := bits.Mul64(hash, uint64(bound))
	return uint32(high & math.MaxUint32)
}

// mixSeed remixes a key for a construction retry. Seed zero returns the id's own
// bytes untouched (already a uniform hash); a nonzero seed reshuffles placement.
func mixSeed(key, seed uint64) uint64 {
	if seed == 0 {
		return key
	}
	h := key ^ (seed * 0x9E3779B97F4A7C15)
	h ^= h >> 33
	h *= 0xff51afd7ed558ccd
	h ^= h >> 33
	h *= 0xc4ceb9fe1a85ec53
	h ^= h >> 33
	return h
}

func (f *binaryFuseFilter) positions(hash uint64) (uint32, uint32, uint32) {
	h0 := mulhiBounded(hash, f.segmentCountLength)
	h1 := h0 + f.segmentLength
	h2 := h1 + f.segmentLength
	// Each offset draws from a different 32-bit window of the hash; the segment
	// mask narrows it to a position inside one segment.
	h1 ^= uint32(hash>>18&math.MaxUint32) & f.segmentLengthMask
	h2 ^= uint32(hash&math.MaxUint32) & f.segmentLengthMask
	return h0, h1, h2
}

// fuseFingerprint folds the hash to the eight bits a slot holds; discarding the
// rest is what sets the 2^-8 false positive rate.
func fuseFingerprint(hash uint64) uint8 {
	return uint8((hash ^ (hash >> 32)) & math.MaxUint8)
}

// contains reports whether the key may be in the set: false proves absence,
// true is a hint. See the filter invariant above.
func (f *binaryFuseFilter) contains(key oidKey) bool {
	if f == nil || len(f.fingerprints) == 0 {
		return false
	}
	hash := mixSeed(key.positionWord()^key.fingerprintWord(), f.seed)
	h0, h1, h2 := f.positions(hash)
	got := fuseFingerprint(hash) ^ f.fingerprints[h0] ^ f.fingerprints[h1] ^ f.fingerprints[h2]
	return got == 0
}

// bits reports the resident size of the filter in bits.
func (f *binaryFuseFilter) bits() int {
	if f == nil {
		return 0
	}
	return len(f.fingerprints) * 8
}

func binaryFuseSegmentLength(size uint32) uint32 {
	if size == 0 {
		return 4
	}
	exponent := math.Floor(math.Log(float64(size))/math.Log(3.33) + 2.25)
	length := uint32(1) << uint(exponent)
	return min(length, 1<<18)
}

func binaryFuseSizeFactor(size uint32) float64 {
	if size <= 1 {
		return 0
	}
	return max(1.125, 0.875+0.25*math.Log(1000000)/math.Log(float64(size)))
}

func newBinaryFuseFilter(keys []oidKey) (*binaryFuseFilter, error) {
	count := len(keys)
	if count > binaryFuseMaxKeys {
		return nil, fmt.Errorf("%w: %d keys", errBinaryFuseTooManyKeys, count)
	}
	size := uint32(count)
	filter := &binaryFuseFilter{}
	filter.segmentLength = binaryFuseSegmentLength(size)
	filter.segmentLengthMask = filter.segmentLength - 1

	sizeFactor := binaryFuseSizeFactor(size)
	capacity := uint32(0)
	if size > 1 {
		capacity = uint32(math.Round(float64(size) * sizeFactor))
	}
	initSegmentCount := (capacity+filter.segmentLength-1)/filter.segmentLength - 2
	arrayLength := (initSegmentCount + 2) * filter.segmentLength
	segmentCount := (arrayLength + filter.segmentLength - 1) / filter.segmentLength
	if segmentCount <= 2 {
		segmentCount = 1
	} else {
		segmentCount -= 2
	}
	filter.segmentCount = segmentCount
	arrayLength = filter.arrayLength()
	filter.segmentCountLength = filter.segmentCount * filter.segmentLength
	filter.fingerprints = make([]uint8, arrayLength)

	if size == 0 {
		return filter, nil
	}

	// Peeling scratch: reverseOrder records the peel order and reverseH the slot
	// each key peeled at; replaying it backwards makes every key's three slots
	// XOR to its fingerprint.
	alone := make([]uint32, arrayLength)
	t2count := make([]uint8, arrayLength)
	t2hash := make([]uint64, arrayLength)
	reverseOrder := make([]uint64, size+1)
	reverseH := make([]uint8, size)

	for attempt := range binaryFuseMaxAttempts {
		filter.seed = uint64(attempt)
		for i := range t2count {
			t2count[i] = 0
			t2hash[i] = 0
		}

		for _, key := range keys {
			hash := mixSeed(key.positionWord()^key.fingerprintWord(), filter.seed)
			h0, h1, h2 := filter.positions(hash)
			for _, h := range [3]uint32{h0, h1, h2} {
				t2count[h] += 4
				t2hash[h] ^= hash
			}
			// The low two bits of t2count XOR-accumulate which slot a surviving
			// key occupies, so a slot down to one key recovers its index without
			// a list.
			t2count[h1] ^= 1
			t2count[h2] ^= 2
		}

		queueLength := 0
		for i := range arrayLength {
			if t2count[i]>>2 == 1 {
				alone[queueLength] = i
				queueLength++
			}
		}

		stackSize := 0
		for queueLength > 0 {
			queueLength--
			index := alone[queueLength]
			if t2count[index]>>2 != 1 {
				continue
			}
			hash := t2hash[index]
			found := t2count[index] & 3
			reverseH[stackSize] = found
			reverseOrder[stackSize] = hash
			stackSize++

			h0, h1, h2 := filter.positions(hash)
			slots := [3]uint32{h0, h1, h2}
			for slot := range uint8(3) {
				if slot == found {
					continue
				}
				other := slots[slot]
				t2count[other] -= 4
				t2hash[other] ^= hash
				switch slot {
				case 1:
					t2count[other] ^= 1
				case 2:
					t2count[other] ^= 2
				}
				if t2count[other]>>2 == 1 {
					alone[queueLength] = other
					queueLength++
				}
			}
		}

		if stackSize != int(size) {
			continue
		}

		for i := stackSize - 1; i >= 0; i-- {
			hash := reverseOrder[i]
			fingerprint := fuseFingerprint(hash)
			h0, h1, h2 := filter.positions(hash)
			slots := [3]uint32{h0, h1, h2}
			target := slots[reverseH[i]]
			value := fingerprint
			for slot := range uint8(3) {
				if slot == reverseH[i] {
					continue
				}
				value ^= filter.fingerprints[slots[slot]]
			}
			filter.fingerprints[target] = value
		}
		return filter, nil
	}
	return nil, fmt.Errorf("%w after %d attempts for %d keys", errBinaryFuseConstruction, binaryFuseMaxAttempts, size)
}

// arrayLength is the fingerprint-slot count the geometry calls for, and so the
// slice length; decodeBinaryFuseFilter refuses an encoding that disagrees.
func (f *binaryFuseFilter) arrayLength() uint32 {
	return (f.segmentCount + 2) * f.segmentLength
}

// encode serializes the filter so a replica can fetch it beside the pack
// rather than rebuild it from the index.
func (f *binaryFuseFilter) encode() []byte {
	out := make([]byte, 0, 32+len(f.fingerprints))
	out = binary.LittleEndian.AppendUint32(out, binaryFuseMagic)
	out = binary.LittleEndian.AppendUint64(out, f.seed)
	out = binary.LittleEndian.AppendUint32(out, f.segmentLength)
	out = binary.LittleEndian.AppendUint32(out, f.segmentCount)
	out = binary.LittleEndian.AppendUint32(out, f.arrayLength())
	return append(out, f.fingerprints...)
}

const binaryFuseMagic = 0x42465531

var errBinaryFuseDecode = errors.New("binary fuse filter: malformed encoding")

func decodeBinaryFuseFilter(raw []byte) (*binaryFuseFilter, error) {
	const header = 4 + 8 + 4 + 4 + 4
	if len(raw) < header {
		return nil, errBinaryFuseDecode
	}
	if binary.LittleEndian.Uint32(raw[0:4]) != binaryFuseMagic {
		return nil, errBinaryFuseDecode
	}
	filter := &binaryFuseFilter{
		seed:          binary.LittleEndian.Uint64(raw[4:12]),
		segmentLength: binary.LittleEndian.Uint32(raw[12:16]),
		segmentCount:  binary.LittleEndian.Uint32(raw[16:20]),
	}
	length := binary.LittleEndian.Uint32(raw[20:24])
	if filter.segmentLength == 0 || uint64(header)+uint64(length) != uint64(len(raw)) {
		return nil, errBinaryFuseDecode
	}
	if uint64(filter.segmentCount+2)*uint64(filter.segmentLength) != uint64(length) {
		return nil, errBinaryFuseDecode
	}
	filter.segmentLengthMask = filter.segmentLength - 1
	filter.segmentCountLength = filter.segmentCount * filter.segmentLength
	filter.fingerprints = append([]uint8(nil), raw[header:]...)
	return filter, nil
}

// ---------------------------------------------------------------------------
// Cuckoo filter, used for the mutable loose-object sets.
// ---------------------------------------------------------------------------

const (
	cuckooSlotsPerBucket = 4
	cuckooMaxKicks       = 500
	// cuckooFingerprintMask keeps 12 bits, ~2^-9 false positives at four slots
	// per bucket.
	cuckooFingerprintMask = 0x0fff
)

// cuckooFilter is the membership structure for a repository's loose objects —
// the mutable tier, where a push adds keys and compaction removes them one at a
// time. A binary fuse filter is built once from a complete set and cannot
// express that; a cuckoo filter deletes natively by clearing the fingerprint it
// stored in one of two candidate buckets.
//
// Deleting a fingerprint that was never inserted could clear a colliding key's
// slot, a false negative the invariant forbids; remove is therefore only ever
// called for a key this process just deleted from the object store.
type cuckooFilter struct {
	buckets [][cuckooSlotsPerBucket]uint16
	mask    uint32
	count   int
	// saturated records that an insertion had nowhere to go. The filter cannot
	// resize (keys were never kept), and dropping the key would answer "absent"
	// for an object that exists; instead a saturated filter stops answering
	// negatively at all, so every probe falls through to the real lookup. The
	// next listing rebuilds it at the needed size and clears the flag.
	saturated bool
}

// cuckooLoadHeadroom sizes the table for twice the counted keys: a four-slot
// bucket only reaches ~95% occupancy before insertions fail, and the directory
// keeps growing after the listing that sized it.
const cuckooLoadHeadroom = 2

// cuckooMaxBuckets is the largest addressable table: a bucket index is a uint32
// masked with buckets-1, so the count must be a power of two fitting a uint32,
// and 2^31 is the last. A larger capacity saturates the filter instead.
const cuckooMaxBuckets = 1 << 31

func newCuckooFilter(capacity int) *cuckooFilter {
	needed := max(capacity, 1)*cuckooLoadHeadroom/cuckooSlotsPerBucket + 1
	buckets := uint32(1)
	for buckets < cuckooMaxBuckets && int(buckets) < needed {
		buckets <<= 1
	}
	return &cuckooFilter{buckets: make([][cuckooSlotsPerBucket]uint16, buckets), mask: buckets - 1}
}

// cuckooFingerprint reads twelve bits of the object id. Zero means "empty slot",
// so an all-zero fingerprint is nudged to one; the merge costs negligible false
// positive rate and never a false negative.
func cuckooFingerprint(key oidKey) uint16 {
	fingerprint := uint16(key.fingerprintWord() & cuckooFingerprintMask)
	if fingerprint == 0 {
		fingerprint = 1
	}
	return fingerprint
}

// index1 is the first of a key's two candidate buckets.
func (c *cuckooFilter) index1(key oidKey) uint32 {
	return uint32(key.positionWord()&math.MaxUint32) & c.mask
}

// altIndex derives the other candidate bucket from the fingerprint alone, so
// either bucket recovers the pair — the filter never stores the key, so
// relocating an occupant must work from its fingerprint.
func (c *cuckooFilter) altIndex(index uint32, fingerprint uint16) uint32 {
	mixed := uint32(fingerprint) * 0x5bd1e995
	mixed ^= mixed >> 15
	return (index ^ mixed) & c.mask
}

func (c *cuckooFilter) insert(key oidKey) {
	if c.saturated {
		return
	}
	fingerprint := cuckooFingerprint(key)
	i1 := c.index1(key)
	i2 := c.altIndex(i1, fingerprint)
	if c.placeIn(i1, fingerprint) || c.placeIn(i2, fingerprint) {
		c.count++
		return
	}

	index := i1
	victim := fingerprint
	for range cuckooMaxKicks {
		slot := int(victim) % cuckooSlotsPerBucket
		victim, c.buckets[index][slot] = c.buckets[index][slot], victim
		index = c.altIndex(index, victim)
		if c.placeIn(index, victim) {
			c.count++
			return
		}
	}
	// The eviction chain ran out with the last victim held nowhere; its key is
	// unknowable, so keep the invariant by ceasing negative answers.
	c.saturated = true
}

func (c *cuckooFilter) placeIn(index uint32, fingerprint uint16) bool {
	for slot := range cuckooSlotsPerBucket {
		if c.buckets[index][slot] == 0 {
			c.buckets[index][slot] = fingerprint
			return true
		}
	}
	return false
}

// contains reports whether the key may be in the set: false proves absence,
// true is a hint. See the filter invariant above.
func (c *cuckooFilter) contains(key oidKey) bool {
	if c == nil {
		return false
	}
	if c.saturated {
		return true
	}
	fingerprint := cuckooFingerprint(key)
	i1 := c.index1(key)
	if c.hasIn(i1, fingerprint) {
		return true
	}
	return c.hasIn(c.altIndex(i1, fingerprint), fingerprint)
}

func (c *cuckooFilter) hasIn(index uint32, fingerprint uint16) bool {
	for slot := range cuckooSlotsPerBucket {
		if c.buckets[index][slot] == fingerprint {
			return true
		}
	}
	return false
}

// remove clears one slot holding the key's fingerprint. Valid only for a key
// this process inserted; see the type comment.
func (c *cuckooFilter) remove(key oidKey) {
	if c == nil || c.saturated {
		return
	}
	fingerprint := cuckooFingerprint(key)
	i1 := c.index1(key)
	if c.clearIn(i1, fingerprint) {
		c.count--
		return
	}
	if c.clearIn(c.altIndex(i1, fingerprint), fingerprint) {
		c.count--
	}
}

func (c *cuckooFilter) clearIn(index uint32, fingerprint uint16) bool {
	for slot := range cuckooSlotsPerBucket {
		if c.buckets[index][slot] == fingerprint {
			c.buckets[index][slot] = 0
			return true
		}
	}
	return false
}
