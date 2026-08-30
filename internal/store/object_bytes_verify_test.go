package store

import (
	"crypto/sha256"
	"errors"
	"io"
	"strings"
	"testing"
)

// drain reads rc to completion and returns the bytes and the terminating error
// (io.EOF on success, or a non-EOF error the reader injected).
func drain(t *testing.T, rc io.ReadCloser) ([]byte, error) {
	t.Helper()
	defer rc.Close()
	var out []byte
	buf := make([]byte, 3) // small buffer to force multiple reads
	for {
		n, err := rc.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			if err == io.EOF {
				return out, nil
			}
			return out, err
		}
	}
}

func TestVerifyingReadCloserPassesMatchingChecksum(t *testing.T) {
	data := []byte("durable streamed object bytes")
	sum := sha256.Sum256(data)
	v := &verifyingReadCloser{rc: io.NopCloser(strings.NewReader(string(data))), hasher: sha256.New(), expected: sum[:], key: "k"}
	got, err := drain(t, v)
	if err != nil {
		t.Fatalf("matching checksum errored: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("streamed bytes = %q, want %q", got, data)
	}
}

func TestVerifyingReadCloserRejectsCorruptedStream(t *testing.T) {
	original := []byte("durable streamed object bytes")
	sum := sha256.Sum256(original)                       // checksum of the ORIGINAL...
	corrupted := []byte("durable streamed object BYTES") // ...but the stream serves corrupted bytes
	v := &verifyingReadCloser{rc: io.NopCloser(strings.NewReader(string(corrupted))), hasher: sha256.New(), expected: sum[:], key: "actions/logs/7/data"}
	got, err := drain(t, v)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("corrupt stream error = %v, want a checksum-mismatch error", err)
	}
	// All bytes are still delivered before the terminating error — the point is
	// the caller learns the response was bad, not that we withhold bytes we can't
	// un-send.
	if string(got) != string(corrupted) {
		t.Fatalf("delivered %q, want %q", got, corrupted)
	}
	if !strings.Contains(err.Error(), "actions/logs/7/data") {
		t.Fatalf("error should name the key: %v", err)
	}
}

func TestVerifyingReadCloserSkipsLegacyObjects(t *testing.T) {
	data := []byte("legacy object with no stored checksum")
	v := &verifyingReadCloser{rc: io.NopCloser(strings.NewReader(string(data))), hasher: sha256.New(), expected: nil, key: "k"}
	got, err := drain(t, v)
	if err != nil {
		t.Fatalf("legacy object errored: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("legacy bytes = %q, want %q", got, data)
	}
}

// The terminating error must be a normal error value (not io.EOF) so callers
// ranging on io.EOF see a failure.
func TestVerifyingReadCloserErrorIsNotEOF(t *testing.T) {
	sum := sha256.Sum256([]byte("x"))
	v := &verifyingReadCloser{rc: io.NopCloser(strings.NewReader("y")), hasher: sha256.New(), expected: sum[:], key: "k"}
	_, err := drain(t, v)
	if errors.Is(err, io.EOF) {
		t.Fatalf("mismatch error must not be io.EOF")
	}
}
