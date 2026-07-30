package bleephub

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var routedPage = regexp.MustCompile(`<Route\s+path="([^"]+)"\s+element=\{<([A-Z][A-Za-z0-9]+)`)
var nativeConfirm = regexp.MustCompile(`\b(?:window\.)?confirm\s*\(`)

func TestEveryUIRouteHasAnExecutableJourney(t *testing.T) {
	app, err := os.ReadFile("../../web/src/App.tsx")
	if err != nil {
		t.Fatal(err)
	}
	var testSources strings.Builder
	for _, root := range []string{"../../web/src/__tests__", "../../web/e2e"} {
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || (!strings.Contains(entry.Name(), ".test.") && !strings.Contains(entry.Name(), ".spec.")) {
				return nil
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			testSources.Write(body)
			testSources.WriteByte('\n')
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	var missing []string
	for _, match := range routedPage.FindAllStringSubmatch(string(app), -1) {
		path, component := match[1], match[2]
		if component == "Navigate" || component == "LoginRedirect" {
			continue
		}
		renderedComponent := regexp.MustCompile(`<` + regexp.QuoteMeta(component) + `(?:\s|/|>)`)
		if !renderedComponent.MatchString(testSources.String()) {
			missing = append(missing, path+" → "+component)
		}
	}
	if len(missing) != 0 {
		t.Fatalf("UI routes without a test that renders their page component:\n  %s", strings.Join(missing, "\n  "))
	}
}

func TestNativeConfirmCannotReenterUIJourneys(t *testing.T) {
	var offenders []string
	err := filepath.WalkDir("../../web/src/pages", func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || (!strings.HasSuffix(path, ".tsx") && !strings.HasSuffix(path, ".ts")) {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if nativeConfirm.Match(body) {
			offenders = append(offenders, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) != 0 {
		t.Fatalf("native confirm dialogs bypass the accessible UI confirmation journey:\n  %s", strings.Join(offenders, "\n  "))
	}
}
