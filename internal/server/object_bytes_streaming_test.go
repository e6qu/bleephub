package bleephub

import (
	"context"
	"io"
	"strings"
	"testing"
)

// STORE-019: the object byte store is no longer []byte-only — PutStream and
// GetStream move data without the whole object residing in memory, and they
// interoperate with the []byte forms.
func TestByteStoreStreamingRoundTrip(t *testing.T) {
	bs := &flakyByteStore{blobs: map[string][]byte{}, failOn: 0}
	ctx := context.Background()

	readAll := func(key string) string {
		t.Helper()
		rc, err := bs.GetStream(ctx, key)
		if err != nil {
			t.Fatalf("GetStream %s: %v", key, err)
		}
		defer rc.Close()
		got, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("read %s: %v", key, err)
		}
		return string(got)
	}

	// PutStream → GetStream.
	if err := bs.PutStream(ctx, "k1", strings.NewReader("hello streaming")); err != nil {
		t.Fatalf("PutStream: %v", err)
	}
	if got := readAll("k1"); got != "hello streaming" {
		t.Fatalf("PutStream/GetStream = %q", got)
	}

	// []byte Put → GetStream.
	if err := bs.Put(ctx, "k2", []byte("via put")); err != nil {
		t.Fatal(err)
	}
	if got := readAll("k2"); got != "via put" {
		t.Fatalf("Put/GetStream = %q", got)
	}

	// PutStream → []byte Get.
	if err := bs.PutStream(ctx, "k3", strings.NewReader("via stream")); err != nil {
		t.Fatal(err)
	}
	got, err := bs.Get(ctx, "k3")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "via stream" {
		t.Fatalf("PutStream/Get = %q", got)
	}
}
