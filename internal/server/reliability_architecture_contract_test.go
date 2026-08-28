package bleephub

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	directPersistenceWrite = regexp.MustCompile(`\.Must(?:Put|Delete)\(`)
	// ARCH-001 moved the data layer to internal/store, where the helper is the
	// exported NewPersistBatch; the server-side alias keeps the old spelling.
	batchedPersistenceWrite = regexp.MustCompile(`\b(?:new|New)PersistBatch\(`)
	sharedHarnessReference  = regexp.MustCompile(`\b(?:testServer|testBaseURL|testSSHAddr)\b`)
)

// TestFocusedRouteTestsUseTheProductionPipeline prevents the narrow
// ghHeadersMiddleware-only fixture from returning, which skipped routing, auth,
// replica refresh, recovery, limits, observation, logging and telemetry; the
// rate-limit test is the one intentional exception.
func TestFocusedRouteTestsUseTheProductionPipeline(t *testing.T) {
	var offenders []string
	err := filepath.WalkDir(".", func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, "_test.go") ||
			entry.Name() == "rate_limits_test.go" ||
			entry.Name() == "reliability_architecture_contract_test.go" {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(body), ".ghHeadersMiddleware(") {
			offenders = append(offenders, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) != 0 {
		t.Fatalf("route tests bypass the production request pipeline: %s", strings.Join(offenders, ", "))
	}
}

// TestReliabilityDebtOnlyShrinks ratchets the two remaining global migrations:
// adding another direct persistence write or shared-fixture consumer fails
// before review.
func TestReliabilityDebtOnlyShrinks(t *testing.T) {
	const (
		maxDirectPersistenceWrites = 568
		minBatchedMutations        = 24
		// Ratcheted down as the TEST-008 migration moves files off the shared
		// `testServer` onto newIsolatedServer; these must only shrink.
		maxSharedHarnessFiles      = 7
		maxSharedHarnessReferences = 34
	)

	directWrites, batchedMutations := 0, 0
	sharedFiles, sharedReferences := 0, 0
	// The persistence-write debt lives in the data layer, which ARCH-001 moved
	// to internal/store; both packages stay under the same ratchet.
	walk := func(root string) error {
		return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if strings.HasSuffix(path, "_test.go") {
				// The contract test and isolated_server_test.go name the shared-harness
				// symbols only to detect or supersede them (TEST-008), not as usage, so
				// exclude both from the count.
				if entry.Name() == "reliability_architecture_contract_test.go" ||
					entry.Name() == "isolated_server_test.go" {
					return nil
				}
				references := len(sharedHarnessReference.FindAll(body, -1))
				if references > 0 {
					sharedFiles++
					sharedReferences += references
				}
				return nil
			}
			directWrites += len(directPersistenceWrite.FindAll(body, -1))
			batchedMutations += len(batchedPersistenceWrite.FindAll(body, -1))
			return nil
		})
	}
	err := walk(".")
	if err == nil {
		err = walk("../store")
	}
	if err != nil {
		t.Fatal(err)
	}
	if directWrites > maxDirectPersistenceWrites {
		t.Errorf("direct persistence writes = %d, ceiling %d; use one persistBatch for the mutation", directWrites, maxDirectPersistenceWrites)
	}
	if batchedMutations < minBatchedMutations {
		t.Errorf("batched mutations = %d, floor %d; a transaction was replaced by independent writes", batchedMutations, minBatchedMutations)
	}
	if sharedFiles > maxSharedHarnessFiles || sharedReferences > maxSharedHarnessReferences {
		t.Errorf("shared test harness grew to %d files/%d references; ceilings are %d/%d", sharedFiles, sharedReferences, maxSharedHarnessFiles, maxSharedHarnessReferences)
	}
}
