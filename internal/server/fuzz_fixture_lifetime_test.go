package bleephub

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A fuzz target's body runs once, at target set-up; only the closure handed to
// f.Fuzz runs per input. Server, store and fixture values built in the body are
// therefore captured and shared by every execution, which breaks fuzzing in two
// ways at once: the outcome of an input depends on which inputs ran before it,
// so a crash the fuzzer finds may not reproduce from the corpus; and an input
// that deletes or exhausts the seed data silently strips the coverage of every
// input after it, while the target keeps reporting green.
//
// newFuzzFixture, fuzzRoutedServer and graphQLFuzzServer take *testing.T so the
// mistake does not compile through them. This test closes the rest of the door:
// no state constructor of any shape may be called in a fuzz target's body
// before the f.Fuzz call.
//
// It matches call shapes, not a list of known constructors, so a state
// constructor added later is covered without editing this test. It does not
// catch state reached through a helper that hides the construction and returns
// a server; the *testing.T signatures are what make that hard to write.
var (
	// Plain calls that produce a server, store or fixture, or seed one.
	fuzzStateConstructorRe = regexp.MustCompile(`(?i)(server|store|fixture)$|^seed`)
	// Method calls that create or seed state on a receiver.
	fuzzStateMethodRe = regexp.MustCompile(`^(Create|Upsert|Seed|Register|Init)`)
)

func TestFuzzTargetsBuildTheirFixturesPerExecution(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatalf("glob test files: %v", err)
	}
	// The GraphQL resolver layer's fuzz targets moved to internal/graphqlapi
	// with the resolver code (ARCH-003); this guard is source-level, so it
	// keeps covering them from here.
	graphqlFiles, err := filepath.Glob(filepath.Join("..", "graphqlapi", "*_test.go"))
	if err != nil {
		t.Fatalf("glob graphqlapi test files: %v", err)
	}
	files = append(files, graphqlFiles...)
	if len(files) == 0 {
		t.Fatal("no test files found; this guard would pass vacuously")
	}

	targets := 0
	for _, name := range files {
		parsed, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Body == nil || !strings.HasPrefix(fn.Name.Name, "Fuzz") {
				continue
			}
			param := fuzzTargetParam(fn)
			if param == "" {
				continue
			}
			targets++
			checkFuzzTargetBody(t, fset, fn, param)
		}
	}
	// A repository-wide guard that found nothing to check is worse than none.
	if targets < 28 {
		t.Fatalf("found only %d fuzz targets; the guard is no longer discovering them", targets)
	}
	t.Logf("checked fixture lifetime in %d fuzz targets", targets)
}

// fuzzTargetParam returns the name of the *testing.F parameter, or "" if fn is
// not a fuzz target.
func fuzzTargetParam(fn *ast.FuncDecl) string {
	if fn.Type.Params == nil || len(fn.Type.Params.List) != 1 {
		return ""
	}
	field := fn.Type.Params.List[0]
	star, ok := field.Type.(*ast.StarExpr)
	if !ok {
		return ""
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "F" {
		return ""
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "testing" || len(field.Names) != 1 {
		return ""
	}
	return field.Names[0].Name
}

func checkFuzzTargetBody(t *testing.T, fset *token.FileSet, fn *ast.FuncDecl, param string) {
	t.Helper()

	fuzzCalls := 0
	var fuzzCallPos token.Pos = token.NoPos
	for _, stmt := range fn.Body.List {
		ast.Inspect(stmt, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Fuzz" {
				if recv, ok := sel.X.(*ast.Ident); ok && recv.Name == param {
					fuzzCalls++
					if fuzzCallPos == token.NoPos {
						fuzzCallPos = call.Pos()
					}
				}
			}
			return true
		})
	}
	if fuzzCalls != 1 {
		t.Errorf("%s: calls %s.Fuzz %d times, want exactly 1 — a target that never calls it reports PASS without executing anything (%s)",
			fn.Name.Name, param, fuzzCalls, fset.Position(fn.Pos()))
		return
	}

	// Everything lexically before the f.Fuzz call runs once for the whole target.
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || call.Pos() >= fuzzCallPos {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			if fuzzStateConstructorRe.MatchString(fun.Name) {
				t.Errorf("%s: %s() is called before %s.Fuzz, so the state it builds is shared by every execution — move it inside the f.Fuzz closure (%s)",
					fn.Name.Name, fun.Name, param, fset.Position(call.Pos()))
			}
		case *ast.SelectorExpr:
			recv, isIdent := fun.X.(*ast.Ident)
			if isIdent && recv.Name == param {
				return true // f.Add, f.Fatalf, f.Helper
			}
			if fuzzStateMethodRe.MatchString(fun.Sel.Name) {
				t.Errorf("%s: %s is called before %s.Fuzz, so the state it creates is shared by every execution — move it inside the f.Fuzz closure (%s)",
					fn.Name.Name, fun.Sel.Name, param, fset.Position(call.Pos()))
			}
		}
		return true
	})
}
