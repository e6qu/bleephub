package bleephub

import (
	"compress/gzip"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// paginationMarkers are the shared pagination/Link-header primitives a list
// handler (or one of its helpers) must reach for a collection route to be
// considered paginated.
var paginationMarkers = []string{
	"paginateAndLink",
	"parsePagination",
	"filterSince",
	"buildLinkHeader",
	"invalidRESTPaginationQuery",
	"cursorPageByID",
	"cursorPaginate",
	`Set("Link"`,
	`startIndex`,
}

// routeHandlerCall unwraps the decorator chain of a s.route("GET ...", expr)
// registration down to the innermost s.handleX method name.
func routeHandlerCall(expr ast.Expr) string {
	for {
		call, ok := expr.(*ast.CallExpr)
		if !ok {
			break
		}
		if len(call.Args) == 0 {
			return ""
		}
		expr = call.Args[len(call.Args)-1]
	}
	if sel, ok := expr.(*ast.SelectorExpr); ok {
		return sel.Sel.Name
	}
	if id, ok := expr.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// collectPaginationSweep parses the package, maps every registered GET route
// to its handler, and returns the routes whose handler never reaches a
// pagination primitive (transitively, through same-package calls).
func collectPaginationSweep(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}

	type fnInfo struct {
		callees []string
		source  string
	}
	funcs := map[string]*fnInfo{}
	var routes []struct {
		pattern string
		handler string
	}

	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			info := &fnInfo{}
			start := fset.Position(fn.Body.Pos()).Offset
			end := fset.Position(fn.Body.End()).Offset
			info.source = string(src[start:end])
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.CallExpr:
					switch fun := node.Fun.(type) {
					case *ast.SelectorExpr:
						info.callees = append(info.callees, fun.Sel.Name)
					case *ast.Ident:
						info.callees = append(info.callees, fun.Name)
					}
					// route registration?
					if sel, ok := node.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "route" {
						if len(node.Args) == 2 {
							if lit, ok := node.Args[0].(*ast.BasicLit); ok {
								pattern := strings.Trim(lit.Value, `"`)
								if strings.HasPrefix(pattern, "GET ") {
									routes = append(routes, struct {
										pattern string
										handler string
									}{pattern, routeHandlerCall(node.Args[1])})
								}
							}
						}
					}
				}
				return true
			})
			funcs[fn.Name.Name] = info
		}
	}

	// reachesPagination reports whether the named function transitively calls
	// a pagination primitive, with a visited set against cycles.
	var reaches func(name string, seen map[string]bool) bool
	reaches = func(name string, seen map[string]bool) bool {
		if seen[name] {
			return false
		}
		seen[name] = true
		info, ok := funcs[name]
		if !ok {
			return false
		}
		for _, marker := range paginationMarkers {
			if strings.Contains(info.source, marker) {
				return true
			}
		}
		for _, callee := range info.callees {
			if reaches(callee, seen) {
				return true
			}
		}
		return false
	}

	var unpaginated []string
	for _, rt := range routes {
		if rt.handler == "" {
			unpaginated = append(unpaginated, rt.pattern+" (unresolved handler)")
			continue
		}
		if !reaches(rt.handler, map[string]bool{}) {
			unpaginated = append(unpaginated, rt.pattern+" -> "+rt.handler)
		}
	}
	sort.Strings(unpaginated)
	return unpaginated
}

func TestPaginationSweepReport(t *testing.T) {
	if os.Getenv("PAGINATION_SWEEP") == "" {
		t.Skip("report-only; run with PAGINATION_SWEEP=1")
	}
	for _, line := range collectPaginationSweep(t) {
		t.Log(line)
	}
}

// loadSpecPaginatedGETs returns normalized "GET /path" -> true for every
// operation in the vendored GitHub OpenAPI description whose GET declares a
// per_page query parameter (i.e. GitHub paginates it).
func loadSpecPaginatedGETs(t *testing.T) map[string]bool {
	t.Helper()
	f, err := os.Open(vendoredSpecFile)
	if err != nil {
		t.Fatalf("open vendored OpenAPI: %v", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gunzip OpenAPI: %v", err)
	}
	defer gz.Close()
	var doc struct {
		Paths map[string]map[string]struct {
			Parameters []struct {
				Name string `json:"name"`
				In   string `json:"in"`
				Ref  string `json:"$ref"`
			} `json:"parameters"`
		} `json:"paths"`
	}
	if err := json.NewDecoder(gz).Decode(&doc); err != nil {
		t.Fatalf("decode OpenAPI: %v", err)
	}
	paginated := map[string]bool{}
	for path, methods := range doc.Paths {
		get, ok := methods["get"]
		if !ok {
			continue
		}
		for _, p := range get.Parameters {
			if p.Name == "per_page" || strings.HasSuffix(p.Ref, "/per-page") {
				paginated["GET "+normalizePath(path)] = true
			}
		}
	}
	if len(paginated) < 100 {
		t.Fatalf("suspiciously few paginated GET operations: %d", len(paginated))
	}
	return paginated
}

// TestGitHubPaginatedRoutesPaginate is the permanent ratchet for list
// pagination: every registered /api/v3 GET route whose operation the vendored
// GitHub OpenAPI description declares per_page for must reach a pagination
// primitive (paginateAndLink, cursorPageByID, ...) from its handler,
// transitively through same-package calls. A new collection route that
// returns the whole collection fails here before review.
func TestGitHubPaginatedRoutesPaginate(t *testing.T) {
	paginated := loadSpecPaginatedGETs(t)
	var offenders []string
	for _, line := range collectPaginationSweep(t) {
		pattern, _, _ := strings.Cut(line, " -> ")
		path := strings.TrimPrefix(pattern, "GET ")
		if !strings.HasPrefix(path, "/api/v3/") {
			continue
		}
		norm := "GET " + normalizePath(strings.TrimPrefix(path, "/api/v3"))
		if paginated[norm] {
			offenders = append(offenders, line)
		}
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("%d route(s) GitHub paginates return whole collections; route the handler through paginateAndLink (or the cursor variant the endpoint family uses):\n  %s",
			len(offenders), strings.Join(offenders, "\n  "))
	}
}
