package bleephub

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestHandlersNeverReadRepositoryIndexWithoutStoreLock is a mechanical
// ratchet around the process-fatal map race. Store methods may read their own
// index under st.Mu; request and workflow code must either use
// GetRepoByFullName or visibly hold s.store.Mu in the same function.
func TestHandlersNeverReadRepositoryIndexWithoutStoreLock(t *testing.T) {
	fset := token.NewFileSet()

	// The engine files moved to internal/actions (ARCH-002) keep the same
	// `s.store` receiver-field spelling, so the ratchet scans both packages.
	type scanned struct{ dir, name string }
	var files []scanned
	for _, dir := range []string{".", "../actions"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("reading package directory %s: %v", dir, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || strings.HasPrefix(name, "store") {
				continue
			}
			files = append(files, scanned{dir, name})
		}
	}

	guardedReads := 0
	for _, sf := range files {
		name := sf.name
		file, err := parser.ParseFile(fset, filepath.Join(sf.dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			rawRead := false
			holdsStoreLock := false
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				switch node := node.(type) {
				case *ast.SelectorExpr:
					rawRead = rawRead || selectorPath(node) == "s.store.ReposByName"
				case *ast.CallExpr:
					selector, ok := node.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					path := selectorPath(selector)
					holdsStoreLock = holdsStoreLock || path == "s.store.Mu.RLock" || path == "s.store.Mu.Lock"
				}
				return true
			})
			if !rawRead {
				continue
			}
			guardedReads++
			if fn.Name.Name == "collectJobSecretsAndVarsLocked" || fn.Name.Name == "protectedEnvironmentLocked" {
				continue
			}
			if !holdsStoreLock {
				t.Errorf("%s:%d reads s.store.ReposByName without the store lock; use GetRepoByFullName",
					name, fset.Position(fn.Pos()).Line)
			}
		}
	}

	// The assertion would become a green no-op if the AST path matcher broke.
	if guardedReads < 6 {
		t.Fatalf("guard found only %d repository-index reads; expected the known locked readers too", guardedReads)
	}
}

func selectorPath(expr ast.Expr) string {
	switch expr := expr.(type) {
	case *ast.Ident:
		return expr.Name
	case *ast.SelectorExpr:
		prefix := selectorPath(expr.X)
		if prefix == "" {
			return expr.Sel.Name
		}
		return prefix + "." + expr.Sel.Name
	default:
		return ""
	}
}

// TestGetRepoByFullNameSerializesConcurrentMapMutation drives the exact
// create/delete versus lookup collision that used to let a request kill the
// process with "concurrent map read and map write".
func TestGetRepoByFullNameSerializesConcurrentMapMutation(t *testing.T) {
	st := store.NewStore()
	st.SeedDefaultUser()
	owner := st.LookupUserByLogin("admin")
	if owner == nil {
		t.Fatal("seeded admin user is missing")
	}

	const repoName = "repository-index-race"
	const writes = 500
	start := make(chan struct{})
	done := make(chan struct{})
	errs := make(chan error, 1)
	var readers sync.WaitGroup
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			<-start
			for {
				select {
				case <-done:
					return
				default:
					_ = st.GetRepoByFullName("admin/" + repoName)
					runtime.Gosched()
				}
			}
		}()
	}

	close(start)
	for range writes {
		if repo := st.CreateRepo(owner, repoName, "", false); repo == nil {
			errs <- errRepoMutation("create returned nil")
			break
		}
		runtime.Gosched()
		if _, err := st.DeleteRepo(owner.Login, repoName); err != nil {
			errs <- errRepoMutation(err.Error())
			break
		}
	}
	close(done)
	readers.Wait()
	select {
	case err := <-errs:
		t.Fatal(err)
	default:
	}
}

type errRepoMutation string

func (e errRepoMutation) Error() string { return string(e) }
