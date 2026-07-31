package bleephub

import (
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
)

func TestObjectChecksumRejectsCorruptedBytes(t *testing.T) {
	original := []byte("durable object bytes")
	sum := sha256.Sum256(original)
	metadata := map[string]string{
		objectChecksumMetadataKey: base64.RawStdEncoding.EncodeToString(sum[:]),
	}
	if err := verifyObjectChecksum(metadata, original); err != nil {
		t.Fatalf("valid checksum rejected: %v", err)
	}
	if err := verifyObjectChecksum(metadata, []byte("corrupted object bytes")); err == nil ||
		!strings.Contains(err.Error(), "does not match") {
		t.Fatalf("corrupt object checksum error = %v", err)
	}
}

func TestObjectChecksumKeepsLegacyObjectsReadable(t *testing.T) {
	if err := verifyObjectChecksum(nil, []byte("legacy")); err != nil {
		t.Fatalf("legacy object without metadata was rejected: %v", err)
	}
}
