package bleephub

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// PAR-018: the specs/simulator-surfaces/*.md tables cite the handler that serves
// each operation. Those citations drift silently when a handler is renamed or
// consolidated (19 were stale). This test keeps them honest: every handler cited
// in a row that claims an implementation must be a real function in this package.
// A row marked "not implemented" (○) carries no handler citation and is skipped.
func TestSimulatorSurfaceCitationsResolve(t *testing.T) {
	defined := map[string]bool{}
	defRe := regexp.MustCompile(`func (?:\([^)]*\) )?(handle[A-Za-z0-9_]+)`)
	goFiles, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range goFiles {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range defRe.FindAllStringSubmatch(string(b), -1) {
			defined[m[1]] = true
		}
	}

	specFiles, err := filepath.Glob("../../specs/simulator-surfaces/*.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(specFiles) == 0 {
		t.Fatal("no simulator-surface spec files found")
	}
	citeRe := regexp.MustCompile("`(?:[A-Za-z0-9_]+\\.go::)?(handle[A-Za-z0-9_]+)`")
	checked := 0
	for _, f := range specFiles {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, "○") {
				continue // an explicitly not-implemented row cites no handler
			}
			for _, m := range citeRe.FindAllStringSubmatch(line, -1) {
				checked++
				if !defined[m[1]] {
					t.Errorf("%s:%d cites handler %s, which is not defined in the package", filepath.Base(f), i+1, m[1])
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no handler citations found — the citation regex or spec format changed")
	}
}
