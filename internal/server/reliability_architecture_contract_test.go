package bleephub

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	directPersistenceWrite  = regexp.MustCompile(`\.Must(?:Put|Delete)\(`)
	batchedPersistenceWrite = regexp.MustCompile(`\bnewPersistBatch\(`)
	sharedHarnessReference  = regexp.MustCompile(`\b(?:testServer|testBaseURL|testSSHAddr)\b`)
)

// TestFocusedRouteTestsUseTheProductionPipeline prevents the narrow
// ghHeadersMiddleware-only fixture from returning. Such tests skipped prefix
// routing, internal authentication, replica refresh, recovery, request limits,
// response observation, logging and telemetry. The rate-limit test is the one
// intentional unit test of ghHeadersMiddleware itself.
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

// TestReliabilityDebtOnlyShrinks turns the two remaining global migrations
// into ratchets. Follow-up conversions lower these ceilings in the same commit;
// adding another direct persistence write or another shared-fixture consumer
// fails before review.
func TestReliabilityDebtOnlyShrinks(t *testing.T) {
	const (
		maxDirectPersistenceWrites = 568
		minBatchedMutations        = 24
		// Ratcheted down as the TEST-008 migration moves files off the shared
		// `testServer` onto per-test isolated servers (newIsolatedServer). Lower
		// these as more files are converted; they must only shrink.
		maxSharedHarnessFiles      = 53
		maxSharedHarnessReferences = 369
	)

	directWrites, batchedMutations := 0, 0
	sharedFiles, sharedReferences := 0, 0
	err := filepath.WalkDir(".", func(path string, entry os.DirEntry, err error) error {
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
			// The contract test names the shared-harness symbols to detect them,
			// and isolated_server_test.go is the migration infrastructure that
			// replaces the shared harness (TEST-008) — it references the shared
			// symbols only to document what it supersedes. Neither is shared-
			// harness *usage*, so exclude both from the count.
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
