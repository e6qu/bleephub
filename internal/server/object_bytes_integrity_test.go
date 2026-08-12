package bleephub

import (
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

func TestObjectChecksumRejectsCorruptedBytes(t *testing.T) {
	original := []byte("durable object bytes")
	sum := sha256.Sum256(original)
	metadata := map[string]string{
		store.ObjectChecksumMetadataKey: base64.RawStdEncoding.EncodeToString(sum[:]),
	}
	if err := store.VerifyObjectChecksum(metadata, original); err != nil {
		t.Fatalf("valid checksum rejected: %v", err)
	}
	if err := store.VerifyObjectChecksum(metadata, []byte("corrupted object bytes")); err == nil ||
		!strings.Contains(err.Error(), "does not match") {
		t.Fatalf("corrupt object checksum error = %v", err)
	}
}

func TestObjectChecksumKeepsLegacyObjectsReadable(t *testing.T) {
	if err := store.VerifyObjectChecksum(nil, []byte("legacy")); err != nil {
		t.Fatalf("legacy object without metadata was rejected: %v", err)
	}
}
