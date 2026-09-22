package store

import (
	"sort"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
)

// TestRememberedDiffCountsAreTheCountedOnes pins GitCommitDiffStats' memo: the
// counts it answers the second time are the ones counting gives, and they are
// remembered once counted.
func TestRememberedDiffCountsAreTheCountedOnes(t *testing.T) {
	stor := memory.NewStorage()
	sig := object.Signature{Name: "M", Email: "m@bleephub.invalid", When: time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)}
	commit := func(files map[string]string, parents ...*object.Commit) *object.Commit {
		t.Helper()
		var entries []object.TreeEntry
		for name, body := range files {
			encoded := stor.NewEncodedObject()
			encoded.SetType(plumbing.BlobObject)
			writer, _ := encoded.Writer()
			_, _ = writer.Write([]byte(body))
			_ = writer.Close()
			hash, err := stor.SetEncodedObject(encoded)
			if err != nil {
				t.Fatal(err)
			}
			entries = append(entries, object.TreeEntry{Name: name, Mode: filemode.Regular, Hash: hash})
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
		encodedTree := stor.NewEncodedObject()
		if err := (&object.Tree{Entries: entries}).Encode(encodedTree); err != nil {
			t.Fatal(err)
		}
		tree, err := stor.SetEncodedObject(encodedTree)
		if err != nil {
			t.Fatal(err)
		}
		c := &object.Commit{Author: sig, Committer: sig, Message: "m", TreeHash: tree}
		for _, parent := range parents {
			c.ParentHashes = append(c.ParentHashes, parent.Hash)
		}
		encodedCommit := stor.NewEncodedObject()
		if err := c.Encode(encodedCommit); err != nil {
			t.Fatal(err)
		}
		hash, err := stor.SetEncodedObject(encodedCommit)
		if err != nil {
			t.Fatal(err)
		}
		read, err := object.GetCommit(stor, hash)
		if err != nil {
			t.Fatal(err)
		}
		return read
	}
	first := commit(map[string]string{"a.txt": "one\ntwo\n", "b.txt": "b\n"})
	second := commit(map[string]string{"a.txt": "one\nthree\nfour\n", "c.txt": "c\n"}, first)
	for _, c := range []*object.Commit{first, second} {
		wantAdds, wantDels, wantFiles, err := countCommitDiff(c)
		if err != nil {
			t.Fatal(err)
		}
		for pass := range 2 {
			adds, dels, files, err := GitCommitDiffStats(c)
			if err != nil || adds != wantAdds || dels != wantDels || files != wantFiles {
				t.Fatalf("pass %d: +%d -%d %d files (%v), counting gives +%d -%d %d", pass, adds, dels, files, err, wantAdds, wantDels, wantFiles)
			}
		}
		if _, remembered := commitDiffCounts.Get(c.Hash); !remembered {
			t.Fatal("a counted commit was not remembered")
		}
	}
	if adds, dels, files, _ := GitCommitDiffStats(second); adds != 3 || dels != 2 || files != 3 {
		t.Fatalf("premise: the second commit counts +%d -%d %d files, want +3 -2 3", adds, dels, files)
	}
}
