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
	// RemoveOldLayout asks for the second step instead of the first: deleting the
	// reference objects and supersession markers of repositories that already
	// have a manifest.
	RemoveOldLayout bool
}

// Adopt runs the one-off operation that brings an object store written before
// the manifest existed up to it, against the store the environment names. For
// each repository it writes the manifest that says what the old layout said —
// which packs were live, which were superseded and when, and what every
// reference held — and reports what it did on out. A repository that already
// has a manifest is reported and left alone; any other failure ends the run.
// With RemoveOldLayout it instead deletes what only the old layout used, from
// repositories that have a manifest. The server itself never reads the old
// layout: it refuses to start on a repository without a manifest.
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
			if _, err := fmt.Fprintf(out, "%s: refused, it already has a manifest\n", repository); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		for _, report := range reports {
			if _, err := fmt.Fprintf(out, "%s: manifest written with %d live packs, %d retired packs and %d references%s\n",
				report.Repository, report.Packs, report.Retired, report.References, snapshotNote(report.Snapshot)); err != nil {
				return err
			}
		}
	}
	return nil
}

func snapshotNote(snapshot string) string {
	if snapshot == "" {
		return ""
	}
	return " (folded into " + snapshot + ")"
}
