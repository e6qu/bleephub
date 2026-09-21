package gitstore

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
)

// A pack is stored as two objects: the pack, exactly as git reads it, and its
// sidecar, which holds everything a reader needs to find its way into the pack.
// The pack stands alone because it is handed to clients as it is — a presigned
// URL for a packfile-URI or a bundle carries no byte range, and git refuses a
// pack with anything after its checksum ("pack has junk at the end") — so what
// describes it cannot be appended to it. Everything else goes in the one
// sidecar, so that a write is two uploads and a commit, and a replica meeting a
// pack for the first time reads one object to know it:
//
//	index   the pack's .idx, version 2, exactly as git writes one
//	filter  the membership filter (filter.go), empty for a pack without one
//	footer  sidecarFooterSize bytes: the magic, the lengths of the two parts,
//	        and the checksum of the pack they describe
//
// The footer makes the sidecar self-describing: the manifest records the same
// lengths so that a reader addresses the parts without reading it first, and a
// reader that loads the index checks the footer against the manifest and the
// pack's name, so a sidecar that is not the one the manifest means is refused
// rather than trusted.

const (
	sidecarSuffix = ".sidecar"
	// sidecarMagic opens the footer, and names the format.
	sidecarMagic = "BHSIDE01"
	// sidecarFooterSize is the magic, two big-endian lengths, and a checksum.
	sidecarFooterSize = len(sidecarMagic) + 8 + 8 + hashSize
)

var errSidecar = errors.New("malformed pack sidecar")

// encodeSidecar lays out a sidecar for the pack whose checksum is pack.
func encodeSidecar(index, filter []byte, pack plumbing.Hash) []byte {
	encoded := make([]byte, 0, len(index)+len(filter)+sidecarFooterSize)
	encoded = append(encoded, index...)
	encoded = append(encoded, filter...)
	encoded = append(encoded, sidecarMagic...)
	encoded = binary.BigEndian.AppendUint64(encoded, uint64(len(index)))
	encoded = binary.BigEndian.AppendUint64(encoded, uint64(len(filter)))
	return append(encoded, pack[:]...)
}

// sidecarFooter is what a footer says.
type sidecarFooter struct {
	indexBytes, filterBytes int64
	pack                    plumbing.Hash
}

// decodeSidecarFooter reads the footer at the end of footer's bytes.
func decodeSidecarFooter(footer []byte) (sidecarFooter, error) {
	if len(footer) != sidecarFooterSize || !bytes.Equal(footer[:len(sidecarMagic)], []byte(sidecarMagic)) {
		return sidecarFooter{}, fmt.Errorf("%w: its footer does not begin %q", errSidecar, sidecarMagic)
	}
	rest := footer[len(sidecarMagic):]
	indexBytes, filterBytes := binary.BigEndian.Uint64(rest[:8]), binary.BigEndian.Uint64(rest[8:16])
	if indexBytes > 1<<62 || filterBytes > 1<<62 {
		return sidecarFooter{}, fmt.Errorf("%w: its footer gives lengths %d and %d", errSidecar, indexBytes, filterBytes)
	}
	decoded := sidecarFooter{indexBytes: int64(indexBytes), filterBytes: int64(filterBytes)}
	copy(decoded.pack[:], rest[16:])
	return decoded, nil
}

// sidecarBytes is the size of a sidecar holding an index and a filter of the
// given sizes.
func sidecarBytes(indexBytes, filterBytes int64) int64 {
	return indexBytes + filterBytes + int64(sidecarFooterSize)
}
