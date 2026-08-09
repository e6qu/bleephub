package bleephub

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// CORE-009: every request-body cap is registered in one auditable place
// (requestBodyLimits), and no production handler may bound a body with a bare
// numeric literal — a magic-number cap is invisible to that registry.
func TestRequestBodyLimits(t *testing.T) {
	seen := map[string]bool{}
	for _, l := range requestBodyLimits {
		if l.bytes <= 0 {
			t.Errorf("body limit %q is not positive: %d", l.name, l.bytes)
		}
		if l.scope == "" {
			t.Errorf("body limit %q has no scope", l.name)
		}
		if seen[l.name] {
			t.Errorf("duplicate body-limit name %q", l.name)
		}
		seen[l.name] = true
	}
	if maxJSONBodyBytes > maxStructuredRequestBody {
		t.Errorf("json cap (%d) exceeds the shared structured cap (%d)", maxJSONBodyBytes, maxStructuredRequestBody)
	}
	if maxUploadBytes <= maxStructuredRequestBody {
		t.Errorf("binary upload cap (%d) should exceed the structured cap (%d)", maxUploadBytes, maxStructuredRequestBody)
	}

	// A production MaxBytesReader must take a named cap constant (a variable /
	// expression like `declared+1` is fine), never a bare numeric literal, so
	// every static limit is one the registry above accounts for.
	goFiles, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	magic := regexp.MustCompile(`MaxBytesReader\([^,]+,[^,]+,\s*(\d[-0-9xa-fA-F<>* _]*)\)`)
	for _, f := range goFiles {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range magic.FindAllStringSubmatch(string(b), -1) {
			t.Errorf("%s: MaxBytesReader uses a magic-number cap %q — use a named constant registered in requestBodyLimits", f, strings.TrimSpace(m[1]))
		}
	}
}
