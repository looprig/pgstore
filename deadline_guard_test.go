package pgstore

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
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

// TestSeamOperationsReturnNotImplemented enforces the invariant P1.1 shipped
// and P1.2 must not lose: an operation that is not implemented yet may never
// hand a caller a nil or zero value without an error. The unimplemented set is
// derived, never hand-listed, so it cannot go stale when P1.3 or P1.4 lands:
//
//   - each internal primitive package declares which storage interface it
//     implements with a var _ storage.X = (*Store)(nil) assertion;
//   - the integration conformance suite skips exactly the primitives whose
//     implementation is still owed by a later task.
//
// A primitive whose conformance suite is skipped must return
// guard.NotImplemented from every operation; a primitive whose conformance
// suite runs must return it from none. Implementing a primitive without
// unskipping its conformance suite, or unskipping without implementing, fails
// here or there.
func TestSeamOperationsReturnNotImplemented(t *testing.T) {
	t.Parallel()

	skipped := conformanceSkips(t)
	packages := primitivePackages(t)
	if len(packages) == 0 {
		t.Fatal("no internal primitive packages declare a storage interface assertion")
	}

	checked := 0
	for _, primitive := range slices.Sorted(maps.Keys(packages)) {
		isSeam, ok := skipped[primitive]
		if !ok {
			t.Errorf("primitive %q has no Test%sConformance entry point; its implementation status cannot be derived", primitive, primitive)
			continue
		}
		for _, path := range packages[primitive] {
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
				checked++
				position := fileSet.Position(function.Pos())
				switch calls := callsGuard(function.Body, "NotImplemented"); {
				case isSeam && !calls:
					t.Errorf("%s:%d %s does not call guard.NotImplemented although %s conformance is skipped as unimplemented", path, position.Line, function.Name.Name, primitive)
				case !isSeam && calls:
					t.Errorf("%s:%d %s calls guard.NotImplemented although %s conformance runs as implemented", path, position.Line, function.Name.Name, primitive)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no exported context-taking primitive methods found")
	}
}

// primitivePackages maps each storage interface to the production files of the
// internal package that declares it implemented.
func primitivePackages(t *testing.T) map[string][]string {
	t.Helper()
	paths, err := filepath.Glob("internal/*/*.go")
	if err != nil {
		t.Fatalf("glob primitive sources: %v", err)
	}
	packages := make(map[string][]string)
	directories := make(map[string]string)
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		fileSet := token.NewFileSet()
		file, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		primitive := declaredStoragePrimitive(file)
		if primitive == "" {
			continue
		}
		directory := filepath.Dir(path)
		if existing, ok := directories[primitive]; ok && existing != directory {
			t.Fatalf("storage.%s is asserted in both %s and %s", primitive, existing, directory)
		}
		directories[primitive] = directory
	}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		for primitive, directory := range directories {
			if filepath.Dir(path) == directory {
				packages[primitive] = append(packages[primitive], path)
			}
		}
	}
	return packages
}

// declaredStoragePrimitive returns X from a var _ storage.X = (*Store)(nil)
// interface-satisfaction assertion, or "" when the file declares none.
func declaredStoragePrimitive(file *ast.File) string {
	for _, declaration := range file.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok || general.Tok != token.VAR {
			continue
		}
		for _, spec := range general.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok || len(value.Names) != 1 || value.Names[0].Name != "_" {
				continue
			}
			selector, ok := value.Type.(*ast.SelectorExpr)
			if !ok {
				continue
			}
			qualifier, ok := selector.X.(*ast.Ident)
			if ok && qualifier.Name == "storage" {
				return selector.Sel.Name
			}
		}
	}
	return ""
}

// conformanceSkips reports, per primitive, whether its conformance entry point
// skips itself as owned by a later task. The file carries an integration build
// tag, so it is read as source text rather than compiled into this test.
func conformanceSkips(t *testing.T) map[string]bool {
	t.Helper()
	const path = "conformance_integration_test.go"
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	skips := make(map[string]bool)
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Recv != nil || function.Body == nil {
			continue
		}
		name, found := strings.CutPrefix(function.Name.Name, "Test")
		if !found {
			continue
		}
		primitive, found := strings.CutSuffix(name, "Conformance")
		if !found || primitive == "" {
			continue
		}
		skips[primitive] = callsTestingSkip(function.Body)
	}
	if len(skips) == 0 {
		t.Fatalf("%s declares no Test<Primitive>Conformance entry point", path)
	}
	return skips
}

func callsTestingSkip(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !strings.HasPrefix(selector.Sel.Name, "Skip") {
			return true
		}
		if qualifier, ok := selector.X.(*ast.Ident); ok && qualifier.Name == "t" {
			found = true
		}
		return true
	})
	return found
}
