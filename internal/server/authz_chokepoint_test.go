package bleephub

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// canReadRepoAsUser answers "may this user read this repository", which is the
// wrong question for a request: a request carries a credential, and an
// installation token's user is a bot that can read nothing while the
// installation can read plenty, a user-to-server token is the intersection of
// its bearer and the app's installations rather than the bearer alone.
// viewerCanReadRepo asks the right question, and every request-scoped caller
// has to go through it.
//
// canPushRepo and canAdminRepo are the write half of the same question and were
// left out of this list, which is why the ratchet stayed green while the write
// half regressed: an installation token that merely reached a repository was
// admitted to every push and admin mutation on the GraphQL lane, and to both
// git transports, without anyone asking what the app had been granted. Watching
// only the read predicate watches only the damage that reads can do.
//
// canAdminOrgAsUser and isActiveOrgMemberAsUser are the organization half of
// exactly the same mistake: an app with no installation anywhere created org
// webhooks, and listed an org's installations, because the gate asked only
// whether the bearer was an admin. viewerCanAdminOrg and viewerIsOrgMember ask
// the right question there.
//
// This is enforced mechanically because reviewing it by eye has failed five
// times. Each round converted the call sites it happened to look at, declared
// the class closed, and left dozens untouched — the last one left a
// zero-permission app reading private repositories through five handlers that
// still called the user-scoped predicate directly.
//
// The two files below are the RBAC layer itself and the credential-aware
// wrapper that fans out from it. Nothing else may call them. This list is a
// ratchet: it may shrink, and an addition to it is a deliberate act that has to
// survive review, not an oversight that compiles.
var userScopedPredicateCallers = map[string]bool{
	"rbac.go":          true,
	"gh_apps_perms.go": true,
}

// userScopedPredicates maps each banned callee to the credential-aware
// predicate that replaces it.
var userScopedPredicates = map[string]string{
	"canReadRepoAsUser":       "(*Server).viewerCanReadRepo",
	"canPushRepo":             "(*Server).viewerCanPushRepo or (*Server).viewerHasRepoPermission",
	"canAdminRepo":            "(*Server).viewerCanAdminRepo or (*Server).viewerHasRepoPermission",
	"canAdminOrgAsUser":       "(*Server).viewerCanAdminOrg",
	"isActiveOrgMemberAsUser": "(*Server).viewerIsOrgMember",
}

func TestOnlyTheRBACLayerAsksWhetherAUserCanReadARepo(t *testing.T) {
	type offence struct {
		callee string
		line   int
	}
	offenders := map[string][]offence{}
	fset := token.NewFileSet()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}
	scanned := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		scanned++
		if userScopedPredicateCallers[name] {
			continue
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			ident, ok := call.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			if _, banned := userScopedPredicates[ident.Name]; !banned {
				return true
			}
			offenders[name] = append(offenders[name], offence{callee: ident.Name, line: fset.Position(call.Pos()).Line})
			return true
		})
	}

	// A rename, a build-tag change or a move would otherwise turn this test
	// into a green no-op that watches nothing.
	if scanned < 100 {
		t.Fatalf("scanned only %d non-test files; this package has far more, so the walk is broken", scanned)
	}
	for name := range userScopedPredicateCallers {
		if _, err := os.Stat(name); err != nil {
			t.Errorf("%s is exempted but does not exist: %v", name, err)
		}
	}
	// And the banned names must still be declared: renaming one away would
	// silence this test rather than fix anything.
	declared := map[string]bool{}
	for name := range userScopedPredicateCallers {
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			fn, ok := node.(*ast.FuncDecl)
			if ok && fn.Recv == nil {
				declared[fn.Name.Name] = true
			}
			return true
		})
	}
	for callee := range userScopedPredicates {
		if !declared[callee] {
			t.Errorf("%s is the predicate this test guards but is not declared in %v; the guard is watching nothing", callee, keysOf(userScopedPredicateCallers))
		}
	}

	if len(offenders) == 0 {
		return
	}
	names := make([]string, 0, len(offenders))
	for name := range offenders {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, o := range offenders[name] {
			t.Errorf("%s:%d calls %s directly; use %s, which honours the credential the request carries",
				name, o.line, o.callee, userScopedPredicates[o.callee])
		}
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
