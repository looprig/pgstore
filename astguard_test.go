package pgstore

// This file holds the general AST predicates the source guards in this package
// are built from. They are named after what they inspect, not after the
// primitive that first needed them: OrderedIndex, Ledger, KV and Leaser all
// need "is this the same object", "is this call inside that block", and "which
// receiver executes this statement", and a copy per primitive is how five
// guards drift apart. TestSharedASTPredicates below is their own coverage —
// a shared derivation with unexercised entries is the risk this file creates,
// so every predicate and every member of rowReturningMethods is pinned here.

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// rowReturningMethods are the pgx entry points that can produce a value an
// assertion later reads. Exec, CopyFrom and BeginTx are deliberately absent:
// Exec returns a command tag, CopyFrom returns a row count, and BeginTx returns
// a transaction, so none of them can supply the rows or the plan a guard is
// binding. Excluding them is what lets a body seed with CopyFrom, set planner
// controls with Exec, or read inside an explicit transaction without the guard
// calling a legal shape a violation.
var rowReturningMethods = map[string]bool{
	"Query":     true,
	"QueryRow":  true,
	"QueryFunc": true,
	"SendBatch": true,
}

// rowReturningCallsOn collects the row-returning calls inside root whose
// receiver is the given object, or any transaction bound from it. Binding to a
// receiver rather than counting names over a whole body is what distinguishes
// "this statement executed the thing under test" from "some call somewhere had
// a familiar name".
func rowReturningCallsOn(root ast.Node, receivers []*ast.Ident) []*ast.CallExpr {
	var calls []*ast.CallExpr
	ast.Inspect(root, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || !rowReturningMethods[callSelectorName(call)] {
			return true
		}
		if callReceiverIsAnyOf(call, receivers) {
			calls = append(calls, call)
		}
		return true
	})
	return calls
}

// callReceiverIsAnyOf reports whether the call's receiver is one of the bound
// identifiers, either directly (tx.QueryRow) or through one selector level
// (s.pool.Query).
func callReceiverIsAnyOf(call *ast.CallExpr, receivers []*ast.Ident) bool {
	selector, ok := unparen(call.Fun).(*ast.SelectorExpr)
	if !ok {
		return false
	}
	base := unparen(selector.X)
	if inner, ok := base.(*ast.SelectorExpr); ok {
		base = unparen(inner.X)
	}
	identifier, ok := base.(*ast.Ident)
	if !ok {
		return false
	}
	for _, candidate := range receivers {
		if sameObject(identifier, candidate) {
			return true
		}
	}
	return false
}

// bindingsFromCall collects the identifiers assigned from a call to the named
// method on one of the given receivers, so a transaction opened from the pool
// under guard is tracked as part of the same receiver.
func bindingsFromCall(root ast.Node, method string, receivers []*ast.Ident) []*ast.Ident {
	var bound []*ast.Ident
	ast.Inspect(root, func(node ast.Node) bool {
		assignment, ok := node.(*ast.AssignStmt)
		if !ok || len(assignment.Lhs) == 0 || len(assignment.Rhs) == 0 {
			return true
		}
		call, ok := unparen(assignment.Rhs[0]).(*ast.CallExpr)
		if !ok || callSelectorName(call) != method {
			return true
		}
		if len(receivers) > 0 && !callReceiverIsAnyOf(call, receivers) {
			return true
		}
		if identifier, ok := assignment.Lhs[0].(*ast.Ident); ok {
			bound = append(bound, identifier)
		}
		return true
	})
	return bound
}

// callNames renders the spellings a guard found, for a message that says which
// call it objected to rather than only how many there were.
func callNames(calls []*ast.CallExpr) []string {
	names := make([]string, 0, len(calls))
	for _, call := range calls {
		names = append(names, callSelectorName(call))
	}
	return names
}

// productionGoFiles is every non-test Go file in the module. Walking rather than
// listing means a new package is covered without anyone remembering it.
func productionGoFiles(t *testing.T) []string {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() && entry.Name() == ".git" {
			return filepath.SkipDir
		}
		if !entry.IsDir() && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk production files: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no production Go files found")
	}
	return paths
}

// parseFixture parses guard-test source held in a string.
func parseFixture(t *testing.T, source string) *ast.File {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "fixture.go", source, 0)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return file
}

func unparen(expression ast.Expr) ast.Expr {
	for {
		paren, ok := expression.(*ast.ParenExpr)
		if !ok {
			return expression
		}
		expression = paren.X
	}
}

func sameObject(left, right *ast.Ident) bool {
	if left == nil || right == nil {
		return false
	}
	if left.Obj != nil || right.Obj != nil {
		return left.Obj != nil && left.Obj == right.Obj
	}
	return left == right
}

func nodeContains(root ast.Node, target ast.Node) bool {
	found := false
	ast.Inspect(root, func(node ast.Node) bool {
		if node == target {
			found = true
			return false
		}
		return !found
	})
	return found
}

func selectorCallsNamed(root ast.Node, method string) []*ast.CallExpr {
	var calls []*ast.CallExpr
	ast.Inspect(root, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		methodSelector, ok := unparen(call.Fun).(*ast.SelectorExpr)
		if !ok || methodSelector.Sel.Name != method {
			return true
		}
		calls = append(calls, call)
		return true
	})
	return calls
}

func freeCallsNamed(root ast.Node, name string) []*ast.CallExpr {
	var calls []*ast.CallExpr
	ast.Inspect(root, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if identifier, ok := unparen(call.Fun).(*ast.Ident); ok && identifier.Name == name {
			calls = append(calls, call)
		}
		return true
	})
	return calls
}

func packageCallsNamed(root ast.Node, packageName, name string) []*ast.CallExpr {
	var calls []*ast.CallExpr
	for _, call := range selectorCallsNamed(root, name) {
		selector := unparen(call.Fun).(*ast.SelectorExpr)
		if qualifier, ok := selectorPackage(selector); ok && qualifier == packageName {
			calls = append(calls, call)
		}
	}
	return calls
}

func callSelectorName(call *ast.CallExpr) string {
	selector, ok := unparen(call.Fun).(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	return selector.Sel.Name
}

func addressedIdent(expression ast.Expr) (*ast.Ident, bool) {
	unary, ok := unparen(expression).(*ast.UnaryExpr)
	if !ok || unary.Op != token.AND {
		return nil, false
	}
	identifier, ok := unparen(unary.X).(*ast.Ident)
	return identifier, ok
}

func freeFunctionsNamed(file *ast.File, name string) []*ast.FuncDecl {
	var functions []*ast.FuncDecl
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Recv == nil && function.Name.Name == name && function.Body != nil {
			functions = append(functions, function)
		}
	}
	return functions
}

func keyedField(literal *ast.CompositeLit, name string) (ast.Expr, error) {
	var found ast.Expr
	for _, element := range literal.Elts {
		keyValue, ok := element.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := keyValue.Key.(*ast.Ident)
		if !ok || key.Name != name {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("has duplicate %s fields", name)
		}
		found = keyValue.Value
	}
	if found == nil {
		return nil, fmt.Errorf("has no keyed %s field", name)
	}
	return found, nil
}

func keyedIdentifier(literal *ast.CompositeLit, field string) (string, error) {
	expression, err := keyedField(literal, field)
	if err != nil {
		return "", err
	}
	identifier, ok := unparen(expression).(*ast.Ident)
	if !ok {
		return "", fmt.Errorf("%s is not a plan metadata identifier", field)
	}
	return identifier.Name, nil
}

func isPointerToNamed(expression ast.Expr, name string) bool {
	pointer, ok := unparen(expression).(*ast.StarExpr)
	if !ok {
		return false
	}
	identifier, ok := unparen(pointer.X).(*ast.Ident)
	return ok && identifier.Name == name
}

func isSliceOfNamed(expression ast.Expr, name string) bool {
	array, ok := unparen(expression).(*ast.ArrayType)
	if !ok || array.Len != nil {
		return false
	}
	identifier, ok := unparen(array.Elt).(*ast.Ident)
	return ok && identifier.Name == name
}

func isSelectorCall(expression ast.Expr, method string) bool {
	call, ok := unparen(expression).(*ast.CallExpr)
	if !ok {
		return false
	}
	selector, ok := unparen(call.Fun).(*ast.SelectorExpr)
	return ok && selector.Sel.Name == method
}

func isHelperCall(expression ast.Expr, helper *ast.FuncDecl) bool {
	call, ok := unparen(expression).(*ast.CallExpr)
	if !ok {
		return false
	}
	identifier, ok := unparen(call.Fun).(*ast.Ident)
	return ok && identifier.Obj != nil && identifier.Obj == helper.Name.Obj
}

func selectorPackage(selector *ast.SelectorExpr) (string, bool) {
	if selector == nil {
		return "", false
	}
	identifier, ok := unparen(selector.X).(*ast.Ident)
	if !ok {
		return "", false
	}
	return identifier.Name, true
}

func isNil(expression ast.Expr) bool {
	identifier, ok := unparen(expression).(*ast.Ident)
	return ok && identifier.Name == "nil"
}

func isZeroInteger(expression ast.Expr) bool {
	value, ok := integerConstant(expression)
	return ok && constant.Sign(value) == 0
}

func isNonzeroInteger(expression ast.Expr) bool {
	value, ok := integerConstant(expression)
	return ok && constant.Sign(value) != 0
}

func integerConstant(expression ast.Expr) (constant.Value, bool) {
	literal, ok := unparen(expression).(*ast.BasicLit)
	if !ok || literal.Kind != token.INT {
		return nil, false
	}
	value := constant.MakeFromLiteral(literal.Value, token.INT, 0)
	return value, value.Kind() == constant.Int
}

// TestSharedASTPredicates covers the predicates in this file directly. A shared
// derivation with unexercised entries is the risk extraction creates: half of
// an earlier execution-method set could be deleted without any test noticing,
// after which the deleted spelling passed every guard built on it.
func TestSharedASTPredicates(t *testing.T) {
	t.Parallel()
	file := parseFixture(t, `package fixture
func helper() int { return 1 }
func (s *Store) Read() {
	rows, _ := s.pool.Query(ctx, "a")
	row := s.pool.QueryRow(ctx, "b")
	s.pool.Exec(ctx, "c")
	s.pool.CopyFrom(ctx, ident, cols, src)
	tx, _ := s.pool.BeginTx(ctx, opts)
	tx.Query(ctx, "d")
	other.pool.Query(ctx, "e")
	_ = helper()
	json.Unmarshal(raw, &plan)
	_ = ((rows))
	_ = row
}`)

	var read *ast.FuncDecl
	for _, declaration := range file.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok && function.Name.Name == "Read" {
			read = function
		}
	}
	if read == nil {
		t.Fatal("fixture lost its Read method")
	}
	receiver := read.Recv.List[0].Names[0]

	t.Run("row-returning calls are bound to the receiver", func(t *testing.T) {
		calls := rowReturningCallsOn(read.Body, []*ast.Ident{receiver})
		if got := callNames(calls); !reflect.DeepEqual(got, []string{"Query", "QueryRow"}) {
			t.Fatalf("rowReturningCallsOn(receiver) = %v, want [Query QueryRow]: Exec, CopyFrom and BeginTx return no rows, and other.pool is not this receiver", got)
		}
	})

	t.Run("a transaction opened from the receiver is part of it", func(t *testing.T) {
		bound := bindingsFromCall(read.Body, "BeginTx", []*ast.Ident{receiver})
		if len(bound) != 1 || bound[0].Name != "tx" {
			t.Fatalf("bindingsFromCall(BeginTx) = %v, want [tx]", bound)
		}
		calls := rowReturningCallsOn(read.Body, append([]*ast.Ident{receiver}, bound...))
		if got := callNames(calls); !reflect.DeepEqual(got, []string{"Query", "QueryRow", "Query"}) {
			t.Fatalf("rowReturningCallsOn(receiver+tx) = %v, want the transaction's read included", got)
		}
	})

	t.Run("free and package calls are distinguished", func(t *testing.T) {
		if got := len(freeCallsNamed(read.Body, "helper")); got != 1 {
			t.Errorf("freeCallsNamed(helper) = %d, want 1", got)
		}
		if got := len(packageCallsNamed(read.Body, "json", "Unmarshal")); got != 1 {
			t.Errorf("packageCallsNamed(json.Unmarshal) = %d, want 1", got)
		}
		if got := len(packageCallsNamed(read.Body, "yaml", "Unmarshal")); got != 0 {
			t.Errorf("packageCallsNamed(yaml.Unmarshal) = %d, want 0", got)
		}
		if got := len(selectorCallsNamed(read.Body, "Query")); got != 3 {
			t.Errorf("selectorCallsNamed(Query) = %d, want 3 across every receiver", got)
		}
	})

	t.Run("containment and identity", func(t *testing.T) {
		calls := rowReturningCallsOn(read.Body, []*ast.Ident{receiver})
		if !nodeContains(read.Body, calls[0]) {
			t.Error("nodeContains missed a call inside the body it was given")
		}
		if nodeContains(calls[1], calls[0]) {
			t.Error("nodeContains reported a sibling call as nested")
		}
		if sameObject(receiver, nil) || !sameObject(receiver, receiver) {
			t.Error("sameObject does not identify an identifier with itself, or accepts nil")
		}
	})

	t.Run("addressed identifiers and parentheses", func(t *testing.T) {
		unary := parseFixture(t, "package fixture\nfunc f() { g(&x, y, ((&z))) }")
		call := freeCallsNamed(unary, "g")[0]
		if identifier, ok := addressedIdent(call.Args[0]); !ok || identifier.Name != "x" {
			t.Errorf("addressedIdent(&x) = %v, %v", identifier, ok)
		}
		if _, ok := addressedIdent(call.Args[1]); ok {
			t.Error("addressedIdent accepted a plain identifier")
		}
		if identifier, ok := addressedIdent(call.Args[2]); !ok || identifier.Name != "z" {
			t.Errorf("addressedIdent(((&z))) = %v, %v; parentheses are not semantic", identifier, ok)
		}
	})
}
