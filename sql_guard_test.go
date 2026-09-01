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
func TestTableSuffixesFitTheReservedIdentifierBudget(t *testing.T) {
	t.Parallel()

	suffixes := make(map[string]string)
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() && entry.Name() == ".git" {
			return filepath.SkipDir
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fileSet := token.NewFileSet()
		file, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			binary, ok := node.(*ast.BinaryExpr)
			if !ok || binary.Op != token.ADD || !isTablePrefixExpression(binary.X) {
				return true
			}
			literal, ok := binary.Y.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			value, unquoteErr := strconv.Unquote(literal.Value)
			if unquoteErr != nil {
				return true
			}
			position := fileSet.Position(literal.Pos())
			suffixes[value] = path + ":" + strconv.Itoa(position.Line)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk production files: %v", err)
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
