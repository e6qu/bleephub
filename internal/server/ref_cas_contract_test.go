package bleephub

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// These functions all derive a new ref value from an observed old value. An
// unconditional SetReference in any of them recreates a lost-update window,
// regardless of whether the REST handler checked a blob SHA or merge head
// earlier in the request.
func TestDerivedRefMutationsUseCompareAndSwap(t *testing.T) {
	files := map[string][]string{
		"gh_repos_git.go": {
			"createFileCommitExpected",
			"createFileCommitExpectedGuarded",
			"deleteFileCommit",
			"deleteFileCommitGuarded",
			"handleUpdateRef",
		},
		"gh_repos_compare.go": {
			"performMerge",
			"performMergeCommit",
			"performSquashMerge",
			"performRebaseMerge",
		},
	}
	casDelegates := map[string]string{
		"createFileCommitExpected": "createFileCommitExpectedGuarded",
		"deleteFileCommit":         "deleteFileCommitGuarded",
	}
	for name, functions := range files {
		parsed, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		byName := map[string]*ast.FuncDecl{}
		for _, declaration := range parsed.Decls {
			if function, ok := declaration.(*ast.FuncDecl); ok {
				byName[function.Name.Name] = function
			}
		}
		for _, functionName := range functions {
			function := byName[functionName]
			if function == nil {
				t.Fatalf("%s: mutation function %s disappeared; update the compare-and-swap contract explicitly", name, functionName)
			}
			compareAndSets := 0
			delegated := false
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				if callee, ok := call.Fun.(*ast.Ident); ok && callee.Name == casDelegates[functionName] {
					delegated = true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch selector.Sel.Name {
				case "SetReference":
					t.Errorf("%s:%s uses unconditional SetReference", name, functionName)
				case "CheckAndSetReference":
					compareAndSets++
				}
				return true
			})
			if compareAndSets == 0 && !delegated {
				t.Errorf("%s:%s has no compare-and-set ref write", name, functionName)
			}
		}
	}
}

func TestRefLifecycleMutationsKeepAtomicStorageBoundaries(t *testing.T) {
	expectations := map[string]map[string]string{
		"gh_repos_git.go": {
			"commitRootBranchWithFiles":        "commitRootBranchWithFilesGuarded",
			"commitRootBranchWithFilesGuarded": "initializeRepositoryReferences",
			"handleCreateRef":                  "createReferenceIfAbsent",
		},
		"gh_repos_refs.go": {
			"handleDeleteRef": "removeReferenceCAS",
		},
		"git_http.go": {
			"applyPushCommandAtomic": "CheckAndSetReference",
		},
	}
	for name, functions := range expectations {
		parsed, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		byName := map[string]*ast.FuncDecl{}
		for _, declaration := range parsed.Decls {
			if function, ok := declaration.(*ast.FuncDecl); ok {
				byName[function.Name.Name] = function
			}
		}
		for functionName, requiredCall := range functions {
			function := byName[functionName]
			if function == nil {
				t.Fatalf("%s: lifecycle function %s disappeared; update the atomic-ref contract explicitly", name, functionName)
			}
			foundRequiredCall := false
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch callee := call.Fun.(type) {
				case *ast.Ident:
					if callee.Name == requiredCall {
						foundRequiredCall = true
					}
				case *ast.SelectorExpr:
					if callee.Sel.Name == requiredCall {
						foundRequiredCall = true
					}
					if callee.Sel.Name == "SetReference" || callee.Sel.Name == "RemoveReference" {
						t.Errorf("%s:%s bypasses the atomic ref lifecycle helper with %s", name, functionName, callee.Sel.Name)
					}
				}
				return true
			})
			if !foundRequiredCall {
				t.Errorf("%s:%s does not call %s", name, functionName, requiredCall)
			}
		}
	}
}
