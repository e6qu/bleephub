package gitbackend

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/e6qu/bleephub/gitstore"
)

// AdoptRequest is what an operator asks of `bleephub adopt`.
type AdoptRequest struct {
	// Repository names one repository, owner/repo. Empty means every repository
	// under the git prefix.
	Repository string
	// RemoveOldLayout asks for the second step instead of the first: deleting
	// what only the earlier layouts used — reference objects, supersession
	// markers, separate pack indexes and filters, loose objects — from
	// repositories that have been adopted.
	RemoveOldLayout bool
}

// Adopt runs the one-off operation that brings an object store written by an
// earlier bleephub up to the current layout, against the store the environment
// names. For each repository it writes the manifest that says what the old
// layout said — which packs were live, which were superseded and when, and what
// every reference held — with each pack's index and filter joined into its
// sidecar and any loose objects packed, and reports what it did on out. A
// repository already in the current layout is reported and left alone; any
// other failure ends the run. With RemoveOldLayout it instead deletes what only
// the earlier layouts used, from repositories that have been adopted. The server
// itself never reads an earlier layout: it refuses to start on one.
func Adopt(ctx context.Context, request AdoptRequest, out io.Writer) error {
	settings, err := SettingsFromEnv()
	if err != nil {
		return err
	}
	if settings.GitBucket == "" {
		return fmt.Errorf("adopt: %s is not set, so git storage is not in an object store and there is nothing to adopt", envGitBucket)
	}
	store, err := openConforming(ctx, settings, settings.GitBucket, settings.GitPrefix)
	if err != nil {
		return err
	}
	store.SetBaseContext(ctx)

	repositories := []string{request.Repository}
	if request.Repository == "" {
		if repositories, err = store.Repositories(ctx); err != nil {
			return err
		}
	}
	for _, repository := range repositories {
		if request.RemoveOldLayout {
			removed, err := store.RemoveAdoptedLayout(ctx, repository)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(out, "%s: removed %d keys of the old layout\n", repository, removed); err != nil {
				return err
			}
			continue
		}
		reports, err := store.Adopt(ctx, repository)
		if errors.Is(err, gitstore.ErrAlreadyAdopted) {
			if _, err := fmt.Fprintf(out, "%s: refused, it is already in the current layout\n", repository); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		for _, report := range reports {
			if _, err := fmt.Fprintf(out, "%s: manifest written with %d live packs, %d retired packs and %d references%s%s\n",
				report.Repository, report.Packs, report.Retired, report.References, snapshotNote(report.Snapshot), looseNote(report.Loose)); err != nil {
				return err
			}
		}
	}
	return nil
}

func looseNote(loose int) string {
	if loose == 0 {
		return ""
	}
	return fmt.Sprintf("; %d loose objects packed", loose)
}

func snapshotNote(snapshot string) string {
	if snapshot == "" {
		return ""
	}
	return " (folded into " + snapshot + ")"
}
