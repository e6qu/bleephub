package bleephub

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// TestGoTestsDoNotReadWallClock makes the deterministic-test rule global:
// tests use fixed fixtures or injected clocks, never the date on the machine
// running them. That keeps month/year rollover and midnight from changing an
// assertion.
func TestGoTestsDoNotReadWallClock(t *testing.T) {
	fset := token.NewFileSet()
	err := filepath.WalkDir("../..", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "Now" {
				return true
			}
			identifier, ok := selector.X.(*ast.Ident)
			if ok && identifier.Name == "time" {
				t.Errorf("%s: tests must use a fixed fixture or injected clock, not time.Now", fset.Position(call.Pos()))
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestStoreModulesUseInjectedClock prevents store mutations from quietly
// bypassing the deterministic clock. The only direct wall-clock reads allowed
// in store modules are the two currentTime fallbacks themselves.
func TestStoreModulesUseInjectedClock(t *testing.T) {
	files, err := filepath.Glob("store*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil || function.Name.Name == "currentTime" {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || selector.Sel.Name != "Now" {
					return true
				}
				identifier, ok := selector.X.(*ast.Ident)
				if !ok || identifier.Name != "time" {
					return true
				}
				t.Errorf("%s: store function %s calls time.Now; use the injected currentTime clock", fset.Position(call.Pos()), function.Name.Name)
				return true
			})
		}
	}
}

func TestStoreClockControlsIndependentStoreModules(t *testing.T) {
	st := store.NewStore()
	want := time.Date(2077, time.November, 3, 4, 5, 6, 0, time.UTC)
	replaceStoreClockNow(st, func() time.Time { return want })
	st.SeedDefaultUser()

	user := st.LookupUserByLogin("admin")
	if user == nil || !user.CreatedAt.Equal(want) {
		t.Fatalf("seeded user time = %v, want %v", user, want)
	}
	repo := st.CreateRepo(user, "clock-contract", "", false)
	project := st.ProjectsV2.CreateProject(user.ID, "User", "Clocked project", user.ID)
	if !project.CreatedAt.Equal(want) || !project.UpdatedAt.Equal(want) {
		t.Fatalf("project times = %v / %v, want %v", project.CreatedAt, project.UpdatedAt, want)
	}
	advisory := st.CreateSecurityAdvisory(
		repo.ID,
		user.ID,
		store.CreateAdvisoryReq{Summary: "clock contract", Description: "clock contract", Severity: "low"},
	)
	if advisory == nil || !advisory.CreatedAt.Equal(want) {
		t.Fatalf("advisory time = %v, want %v", advisory, want)
	}
	if ok := st.RequestCVE(advisory.ID); !ok || !strings.HasPrefix(advisory.CVEID, "CVE-2077-") {
		t.Fatalf("CVE ID = %q, want injected year", advisory.CVEID)
	}
}

func TestProjectV2ClockFallbackDoesNotRecurse(t *testing.T) {
	st := store.NewProjectV2Store(nil)
	project := st.CreateProject(1, "User", "Real clock fallback", 1)
	if project.CreatedAt.IsZero() || project.UpdatedAt.IsZero() {
		t.Fatalf("project fallback timestamps are zero: %v / %v", project.CreatedAt, project.UpdatedAt)
	}
}
