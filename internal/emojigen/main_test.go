package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadTwemojiTarballRefusesUnpinnedBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "twemoji.tar.gz")
	if err := os.WriteFile(path, []byte("not the pinned archive"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := readTwemojiTarball(path)
	if err == nil {
		t.Fatal("readTwemojiTarball accepted bytes that do not match the pinned digest")
	}
	for _, want := range []string{path, twemojiTarballSHA256, twemojiCommit, "refusing to vendor unverified bytes"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
}
