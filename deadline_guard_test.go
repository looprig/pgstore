package pgstore

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
)

// TestStructuredOperationMethodsCallDeadlineGuard derives the operation set
// from production source. Every exported method whose first argument is a
// context must call guard.RequireDeadline, so a newly added operation cannot
// silently fall outside the deadline policy.
func TestStructuredOperationMethodsCallDeadlineGuard(t *testing.T) {
	t.Parallel()

	paths, err := filepath.Glob("internal/*/*.go")
	if err != nil {
		t.Fatalf("glob primitive sources: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no primitive production sources found")
	}

	operations := 0
	for _, path := range paths {
		if !slices.Contains([]string{"ledger", "lease", "kv", "orderedindex"}, filepath.Base(filepath.Dir(path))) {
			continue
		}
		fileSet := token.NewFileSet()
		file, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		imports := importNames(t, file)
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Recv == nil || !function.Name.IsExported() || function.Body == nil || !firstParameterIsContext(function, imports) {
				continue
			}
			operations++
			if !callsGuard(function.Body, "RequireDeadline") {
				position := fileSet.Position(function.Pos())
				t.Errorf("%s:%d %s does not call guard.RequireDeadline", path, position.Line, function.Name.Name)
			}
			if !callsGuard(function.Body, "NotImplemented") {
				position := fileSet.Position(function.Pos())
				t.Errorf("%s:%d %s does not call guard.NotImplemented", path, position.Line, function.Name.Name)
			}
		}
	}
	if operations == 0 {
		t.Fatal("no exported context-taking primitive methods found")
	}
}

func importNames(t *testing.T, file *ast.File) map[string]string {
	t.Helper()
	imports := make(map[string]string)
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			t.Fatalf("unquote import: %v", err)
		}
		name := filepath.Base(path)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		imports[name] = path
	}
	return imports
}

func firstParameterIsContext(function *ast.FuncDecl, imports map[string]string) bool {
	if function.Type.Params == nil || len(function.Type.Params.List) == 0 {
		return false
	}
	selector, ok := function.Type.Params.List[0].Type.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Context" {
		return false
	}
	qualifier, ok := selector.X.(*ast.Ident)
	return ok && imports[qualifier.Name] == "context"
}

func callsGuard(body *ast.BlockStmt, method string) bool {
	found := false
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != method {
			return true
		}
		qualifier, ok := selector.X.(*ast.Ident)
		if ok && qualifier.Name == "guard" {
			found = true
		}
		return true
	})
	return found
}
