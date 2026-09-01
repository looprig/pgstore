package pgstore

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestKVKeysPinsBytewiseCollation is a source guard that overlaps, but is not
// subsumed by, the behavioural TestKVKeysUsesMemstoreBytewiseOrdering. Removing
// COLLATE "C" genuinely fails the behavioural test on a database whose
// collation is linguistic, such as the en_US.utf8 default of the postgres:17
// image. It does NOT fail on a database created with LC_COLLATE=C, where the
// behavioural ordering is identical with and without the explicit collation and
// the memstore-parity guarantee would be left entirely unguarded. Since the
// collation of the disposable test database is not a property this repository
// controls, this guard holds the guarantee in the database-free suite.
func TestKVKeysPinsBytewiseCollation(t *testing.T) {
	source, err := os.ReadFile("internal/kv/kv.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(source), `ORDER BY key COLLATE \"C\"`) {
		t.Fatal(`KV.Keys query does not pin ORDER BY key COLLATE "C"`)
	}
}

// TestTableSuffixesFitTheReservedIdentifierBudget derives every table-name
// suffix that production source concatenates onto the validated table prefix
// and holds it inside the budget options.go reserves. PostgreSQL truncates
// identifiers longer than 63 bytes silently, so a long P1.3 or P1.4 suffix
// combined with a maximum-length TablePrefix would not fail: two tables would
// collide on one truncated name. Deriving the suffixes means a new primitive is
// covered without anyone remembering to extend a list.
//
// The shapes the derivation understands are prefix + "literal" and
// prefix + NamedConstant, where the constant is a package-level string constant
// in the same directory. Any other right-hand operand is an error rather than a
// silent omission: a suffix this test cannot read is a suffix it cannot bound.
func TestTableSuffixesFitTheReservedIdentifierBudget(t *testing.T) {
	t.Parallel()

	paths := productionGoFiles(t)
	constants := stringConstantsByDirectory(t, paths)
	suffixes := make(map[string]string)
	for _, path := range paths {
		fileSet := token.NewFileSet()
		file, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			binary, ok := node.(*ast.BinaryExpr)
			if !ok || binary.Op != token.ADD || !isTablePrefixExpression(binary.X) {
				return true
			}
			where := path + ":" + strconv.Itoa(fileSet.Position(binary.Pos()).Line)
			suffix, ok := resolveStringOperand(binary.Y, constants[filepath.Dir(path)])
			if !ok {
				t.Errorf("%s: table suffix operand is not a string literal or a package-level string constant, so its length cannot be bounded here", where)
				return true
			}
			suffixes[suffix] = where
			return true
		})
	}
	if len(suffixes) < 4 {
		t.Fatalf("found %d table suffixes (%v); the derivation pattern no longer matches production source", len(suffixes), suffixes)
	}
	for suffix, where := range suffixes {
		if len(suffix) > maxTableSuffixBytes {
			t.Errorf("%s: table suffix %q is %d bytes, over the %d-byte reserve; a %d-byte TablePrefix would be silently truncated by PostgreSQL", where, suffix, len(suffix), maxTableSuffixBytes, maxTablePrefixBytes)
		}
	}
	if maxTablePrefixBytes+maxTableSuffixBytes != maxPostgresIdentifierBytes {
		t.Fatalf("prefix budget %d plus suffix reserve %d must equal the %d-byte PostgreSQL identifier limit", maxTablePrefixBytes, maxTableSuffixBytes, maxPostgresIdentifierBytes)
	}
}

// resolveStringOperand reads a suffix written either inline or as a named
// constant. Naming the constant must not remove it from the budget.
func resolveStringOperand(expression ast.Expr, constants map[string]string) (string, bool) {
	switch operand := expression.(type) {
	case *ast.BasicLit:
		if operand.Kind != token.STRING {
			return "", false
		}
		value, err := strconv.Unquote(operand.Value)
		return value, err == nil
	case *ast.Ident:
		value, ok := constants[operand.Name]
		return value, ok
	}
	return "", false
}

// stringConstantsByDirectory collects package-level string constants per
// package directory, so a suffix constant declared in one file of a package is
// resolvable from every other file in it.
func stringConstantsByDirectory(t *testing.T, paths []string) map[string]map[string]string {
	t.Helper()
	constants := make(map[string]map[string]string)
	for _, path := range paths {
		fileSet := token.NewFileSet()
		file, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		directory := filepath.Dir(path)
		for _, declaration := range file.Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok || general.Tok != token.CONST {
				continue
			}
			for _, spec := range general.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok || len(value.Names) != 1 || len(value.Values) != 1 {
					continue
				}
				literal, ok := value.Values[0].(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					continue
				}
				unquoted, err := strconv.Unquote(literal.Value)
				if err != nil {
					continue
				}
				if constants[directory] == nil {
					constants[directory] = make(map[string]string)
				}
				constants[directory][value.Names[0].Name] = unquoted
			}
		}
	}
	return constants
}

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

// isTablePrefixExpression reports the validated table-prefix operand in both
// forms production code uses: a local prefix variable and a store field.
func isTablePrefixExpression(expression ast.Expr) bool {
	switch operand := expression.(type) {
	case *ast.Ident:
		return operand.Name == "prefix" || operand.Name == "tablePrefix"
	case *ast.SelectorExpr:
		return operand.Sel.Name == "tablePrefix"
	}
	return false
}
