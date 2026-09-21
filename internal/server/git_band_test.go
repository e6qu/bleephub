package bleephub

import (
	"bytes"
	"strconv"
	"testing"
)

// TestPackDataIsCutIntoPacketsTheProtocolAllows pins the side band's packet
// size. A pack copied out of storage is handed to the band writer an extent at
// a time — megabytes in one write — and every packet must still fit: 65520
// bytes on side-band-64k and 1000 on side-band, the 4-byte length included. The
// pkt-lines are read back by hand, so the check does not rest on the library
// that wrote them.
func TestPackDataIsCutIntoPacketsTheProtocolAllows(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789abcdef"), 200_000/16)
	for _, tc := range []struct {
		name  string
		mode  gitSidebandMode
		limit int
	}{
		{"side-band-64k", gitSideband64k, 65520},
		{"side-band", gitSideband, 1000},
	} {
		var out bytes.Buffer
		band := newGitBandWriter(&out, tc.mode, false)
		if n, err := band.pack().Write(data); err != nil || n != len(data) {
			t.Fatalf("%s: wrote %d of %d bytes: %v", tc.name, n, len(data), err)
		}
		var received []byte
		packets, largest := 0, 0
		for rest := out.Bytes(); len(rest) > 0; packets++ {
			length, err := strconv.ParseUint(string(rest[:4]), 16, 16)
			if err != nil || int(length) > len(rest) || length < 5 {
				t.Fatalf("%s: packet %d has length %q", tc.name, packets, rest[:4])
			}
			if rest[4] != 1 {
				t.Fatalf("%s: packet %d is on band %d, want 1", tc.name, packets, rest[4])
			}
			largest = max(largest, int(length))
			received = append(received, rest[5:length]...)
			rest = rest[length:]
		}
		if largest > tc.limit {
			t.Fatalf("%s: a packet is %d bytes, over the protocol's %d", tc.name, largest, tc.limit)
		}
		if largest != tc.limit {
			t.Fatalf("premise: the largest packet is %d bytes, so the limit of %d was never reached", largest, tc.limit)
		}
		if !bytes.Equal(received, data) {
			t.Fatalf("%s: %d bytes arrived, not the %d written", tc.name, len(received), len(data))
		}
	}
}
