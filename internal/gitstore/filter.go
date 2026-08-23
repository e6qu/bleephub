package gitstore

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/bits"
)

// This file holds the two approximate membership structures the object index
// uses, and the invariant that governs both of them.
//
// FILTER INVARIANT: a filter here may only ever be believed when it says NO.
//
// Both structures are one-sided. A key that was inserted always probes as
// present; a key that was not may still probe as present with a small
// probability. So "absent" is a proof and "present" is a hint, and every caller
// is written so that a "present" answer only ever causes it to do the exact
// lookup it would have done without a filter at all. No filter result may
// decide the outcome of a git operation. An object that a filter said was
// present but is not resolves as not found by the real index; an object that a
// filter said was absent is not looked for at all, and that is the only place a
// filter can do harm — which is why the absent answer must be a proof, and why
// TestSaturatedFilterCannotHideAnObject drives a filter that answers "present"
// to everything through the whole read path and requires identical results.
//
// Object ids are already uniform cryptographic hashes. Both structures derive
// their probe positions straight from the id's own bytes rather than hashing it
// a second time: a SHA-1 or SHA-256 digest is exactly the uniformly distributed
// bit string a filter wants, and re-hashing it would only spend cycles.

// oidKey is the fixed-width view the filters take of an object id. go-git's
// plumbing.Hash is 20 bytes today and 32 under SHA-256; both are read through
// this type so neither filter has to know which.
type oidKey [32]byte

func oidKeyFrom(raw []byte) oidKey {
	var key oidKey
	copy(key[:], raw)
	return key
}

// positionWord is the 64 bits that place a key in the table.
func (k oidKey) positionWord() uint64 { return binary.LittleEndian.Uint64(k[0:8]) }

// fingerprintWord is drawn from bytes the position word does not use, so a
// key's slot and its fingerprint are independent.
func (k oidKey) fingerprintWord() uint64 { return binary.LittleEndian.Uint64(k[8:16]) }

// ---------------------------------------------------------------------------
// Binary fuse filter, used for the immutable per-pack object sets.
// ---------------------------------------------------------------------------

// binaryFuseFilter is a binary fuse filter with 8-bit fingerprints.
//
// It is used for packs, and only for packs, because it is a static structure:
// the key set has to be known in full at construction and no key can be added
// or removed afterwards. That is exactly a packfile. A pack is written once,
// named for the hash of its own contents, and never modified; when compaction
// supersedes it the whole pack and the whole filter are discarded together, so
// the deletion a mutable filter would offer has nothing to do.
//
// It is preferred to a Bloom filter on space. For a target false positive rate
// of 2^-8 the information-theoretic bound is 8 bits per key; a binary fuse
// filter needs about 1.13 times that, roughly 9 bits, while a Bloom filter
// needs 1.44 times it, roughly 11.5 bits — a quarter more memory for the same
// answer, on a structure that is meant to stay resident for every pack of every
// repository a replica serves. It also probes better: a Bloom filter of k=6
// touches six independent words spread across the whole table, one cache miss
// each, whereas a binary fuse filter touches three slots confined to a window
// of three consecutive segments, so a probe is one or two cache lines instead
// of six.
//
// A ribbon filter is slightly smaller still, but its query decodes a banded
// linear system, and the space it saves over a binary fuse filter is under 5%.
// That is not worth a more delicate query path in code whose failure mode is a
// corrupted repository.
type binaryFuseFilter struct {
	seed               uint64
	segmentLength      uint32
	segmentLengthMask  uint32
	segmentCount       uint32
	segmentCountLength uint32
	fingerprints       []uint8
}

// binaryFuseMaxAttempts bounds construction. Peeling succeeds on the first
// attempt with overwhelming probability; the retries exist for the pathological
// key sets that do not peel, and a bound turns "does not terminate" into an
// error the caller can report.
const binaryFuseMaxAttempts = 100

var errBinaryFuseConstruction = errors.New("binary fuse filter: construction did not converge")

// binaryFuseMaxKeys bounds the key set a filter may be built for. Every slot is
// addressed by a uint32 and the table is sized above the key count, so a set
// this large has no table that can hold it. A pack that big is refused outright
// rather than sized from a wrapped count, because a table sized from a wrapped
// count would place keys at positions its own probe never visits, and a key that
// probes as absent is the one answer the filter invariant forbids.
const binaryFuseMaxKeys = math.MaxUint32 / 2

var errBinaryFuseTooManyKeys = errors.New("binary fuse filter: more keys than the table can address")

// mulhiBounded maps a hash uniformly onto [0, bound) by taking the high half of
// the 128-bit product, which is the multiply-shift alternative to a modulo. The
// product's high half is strictly below bound, and bound is a uint32, so the low
// thirty-two bits named here are the whole of the result.
func mulhiBounded(hash uint64, bound uint32) uint32 {
	high, _ := bits.Mul64(hash, uint64(bound))
	return uint32(high & math.MaxUint32)
}

// mixSeed remixes a key for a construction retry. The first attempt uses seed
// zero and returns the object id's own bytes untouched, which is the point:
// the id is already a uniform hash. A retry needs a different placement of the
// same keys, and only then is a mixing step spent.
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
	// Each of the two offsets is drawn from a different thirty-two bit window of
	// the same hash; the segment mask then narrows it to a position inside one
	// segment.
	h1 ^= uint32(hash>>18&math.MaxUint32) & f.segmentLengthMask
	h2 ^= uint32(hash&math.MaxUint32) & f.segmentLengthMask
	return h0, h1, h2
}

// fuseFingerprint folds the hash down to the eight bits a slot holds. The fold
// is the point: the filter stores a truncated hash, and its false positive rate
// is exactly the 2^-8 that discarding the rest buys.
func fuseFingerprint(hash uint64) uint8 {
	return uint8((hash ^ (hash >> 32)) & math.MaxUint8)
}

// contains reports whether the key may be in the set. A false answer is a
// proof of absence; a true answer is a hint. See the filter invariant above.
func (f *binaryFuseFilter) contains(key oidKey) bool {
	if f == nil || len(f.fingerprints) == 0 {
		return false
	}
	hash := mixSeed(key.positionWord()^key.fingerprintWord(), f.seed)
	h0, h1, h2 := f.positions(hash)
	got := fuseFingerprint(hash) ^ f.fingerprints[h0] ^ f.fingerprints[h1] ^ f.fingerprints[h2]
	return got == 0
}

// bits reports the resident size of the filter in bits, for the space
// measurement the report quotes.
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

	// Peeling scratch space. reverseOrder records the order keys were peeled
	// off in and reverseH which of the three slots each was peeled at; walking
	// that record backwards and writing each key's fingerprint into its own
	// slot is what makes every key's three slots XOR to its fingerprint.
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
			// The low two bits of t2count carry which of the three slots a
			// surviving key occupies, accumulated by XOR, so that when a slot
			// is down to one key its index is recoverable without a list.
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

// arrayLength is the number of fingerprint slots the filter's geometry calls
// for, and so the length of the fingerprint slice: the constructor allocates it
// at exactly this size and decodeBinaryFuseFilter refuses an encoding whose
// stated length disagrees with it.
func (f *binaryFuseFilter) arrayLength() uint32 {
	return (f.segmentCount + 2) * f.segmentLength
}

// encode serializes the filter so a replica can fetch it beside the pack
// instead of rebuilding it from the index.
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
	// cuckooFingerprintMask keeps 12 bits, which puts the false positive rate
	// near 2^-9 at four slots per bucket.
	cuckooFingerprintMask = 0x0fff
)

// cuckooFilter is the membership structure for a repository's loose objects.
//
// Loose objects are the mutable tier: a push adds them and compaction takes
// them away one key at a time. A binary fuse filter cannot express that — it is
// built once from a complete key set — and rebuilding one per removal would
// cost more than it saves. A cuckoo filter supports deletion natively, because
// a key's fingerprint is stored explicitly in one of two candidate buckets, so
// removing it is finding that fingerprint and clearing the slot. That is what
// lets compaction retire a hundred thousand loose keys without ever re-listing
// the object store.
//
// Deleting a fingerprint that was never inserted could clear a slot belonging
// to a colliding key, which would turn that key's answer from "present" into
// "absent" — a false negative, and the one thing the filter invariant forbids.
// remove is therefore only ever called for a key this process has itself just
// deleted from the object store, so the fingerprint it clears is one it put
// there.
type cuckooFilter struct {
	buckets [][cuckooSlotsPerBucket]uint16
	mask    uint32
	count   int
	// saturated records that an insertion had nowhere to go. A cuckoo filter
	// cannot be resized, because the keys it was built from were never kept and
	// a fingerprint's bucket in a larger table is not recoverable from the
	// fingerprint alone. Dropping the key instead would make the filter answer
	// "absent" for an object that exists, which the filter invariant forbids
	// outright, so an overflowing filter stops answering negatively at all and
	// every probe falls through to the real lookup. The next listing rebuilds
	// it at the size the directory actually needs and clears the flag.
	saturated bool
}

// cuckooLoadHeadroom sizes the table above the key count it is built for. A
// cuckoo table with four slots per bucket only reaches about 95% occupancy
// before insertions start failing, and a loose-object directory keeps growing
// after the listing that sized it, so the table is built for twice the keys
// that were counted.
const cuckooLoadHeadroom = 2

// cuckooMaxBuckets is the largest table the index can address. A bucket index is
// a uint32 masked with buckets-1, so the count has to be a power of two that
// still fits a uint32, and 2^31 is the last one. A capacity asking for more
// stops here and the filter saturates, which costs lookups and never an answer.
const cuckooMaxBuckets = 1 << 31

func newCuckooFilter(capacity int) *cuckooFilter {
	needed := max(capacity, 1)*cuckooLoadHeadroom/cuckooSlotsPerBucket + 1
	buckets := uint32(1)
	for buckets < cuckooMaxBuckets && int(buckets) < needed {
		buckets <<= 1
	}
	return &cuckooFilter{buckets: make([][cuckooSlotsPerBucket]uint16, buckets), mask: buckets - 1}
}

// fingerprintOf reads twelve bits out of the object id. Zero is reserved to
// mean "empty slot", so a key whose bits are all zero is nudged to one; that
// merges it with the keys whose fingerprint is genuinely one, which costs a
// negligible amount of false positive rate and never a false negative.
func cuckooFingerprint(key oidKey) uint16 {
	fingerprint := uint16(key.fingerprintWord() & cuckooFingerprintMask)
	if fingerprint == 0 {
		fingerprint = 1
	}
	return fingerprint
}

// index1 is the first of a key's two candidate buckets. Only the low
// thirty-two bits of the position word reach the table, and the mask narrows
// them again to the table's own size.
func (c *cuckooFilter) index1(key oidKey) uint32 {
	return uint32(key.positionWord()&math.MaxUint32) & c.mask
}

// altIndex is derived from the fingerprint alone, which is what makes the pair
// of candidate buckets recoverable from either one of them: the filter never
// stores the key, so relocating an occupant has to work from its fingerprint.
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
	// The eviction chain ran out and one fingerprint — the last victim — is
	// now held nowhere. Which key it belonged to is unknowable, so the only
	// answer that keeps the invariant is to stop giving negative answers.
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

// contains reports whether the key may be in the set. A false answer is a
// proof of absence; a true answer is a hint. See the filter invariant above.
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

// remove clears one slot holding the key's fingerprint. It is only valid for a
// key this process inserted; see the type comment.
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
