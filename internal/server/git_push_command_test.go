package bleephub

import (
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/e6qu/bleephub/internal/gitstore"
)

func TestGitPushCommandsHonorWireOldObjectID(t *testing.T) {
	stor := gitstore.WrapAtomicRefStorage("owner/repo", memory.NewStorage())
	name := plumbing.NewBranchReferenceName("main")
	current := plumbing.NewHash("2222222222222222222222222222222222222222")
	if err := stor.SetReference(plumbing.NewHashReference(name, current)); err != nil {
		t.Fatal(err)
	}
	stale := plumbing.NewHash("1111111111111111111111111111111111111111")
	next := plumbing.NewHash("3333333333333333333333333333333333333333")
	for _, command := range []*packp.Command{
		{Name: name, Old: stale, New: next},
		{Name: name, Old: stale, New: plumbing.ZeroHash},
		{Name: name, Old: plumbing.ZeroHash, New: next},
	} {
		if err := applyPushCommandAtomic(stor, command); err == nil {
			t.Fatalf("%s with stale/duplicate precondition unexpectedly succeeded", command.Action())
		}
		got, err := stor.Reference(name)
		if err != nil {
			t.Fatal(err)
		}
		if got.Hash() != current {
			t.Fatalf("%s changed ref to %s, want %s", command.Action(), got.Hash(), current)
		}
	}
}
