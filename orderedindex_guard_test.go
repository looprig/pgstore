package pgstore

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

func TestOrderedIndexQueriesRemainIndexBackedKeysets(t *testing.T) {
	implementation, err := os.ReadFile("internal/orderedindex/orderedindex.go")
	if err != nil {
		t.Fatal(err)
	}
	queries, err := os.ReadFile("internal/orderedquery/orderedquery.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(implementation) + "\n" + string(queries)
	lower := strings.ToLower(source)
	for _, forbidden := range []string{" offset ", "internal/kv", "sort.", "pg_advisory"} {
		if strings.Contains(lower, forbidden) {
			t.Errorf("OrderedIndex production source contains forbidden scan/fallback mechanism %q", forbidden)
		}
	}
	for _, required := range []string{
		"order_id > $3::numeric",
		"(rank_value, stable_key, ordering_scope) < ($3, $4, $5)",
		"(due_at, stable_key, ordering_scope) > ($3, $4, $5)",
		"ORDER BY rank_value DESC, stable_key DESC, ordering_scope DESC",
		"ORDER BY due_at ASC, stable_key ASC, ordering_scope ASC",
	} {
		if !strings.Contains(source, required) {
			t.Errorf("OrderedIndex source lost required keyset fragment %q", required)
		}
	}
}

func TestOrderedIndexPlanGateUsesProductionStatements(t *testing.T) {
	production, err := os.ReadFile("internal/orderedindex/orderedindex.go")
	if err != nil {
		t.Fatal(err)
	}
	planGate, err := os.ReadFile("orderedindex_plan_integration_test.go")
	if err != nil {
		t.Fatal(err)
	}
	if err := validateProductionStatementOwnership(production); err != nil {
		t.Errorf("OrderedIndex production statement ownership: %v", err)
	}
	if err := validatePlanGateStatementOwnership(planGate); err != nil {
		t.Errorf("ordered plan gate statement ownership: %v", err)
	}
}

func TestPlanGateStatementOwnershipRejectsCopiesAndUnusedBuilders(t *testing.T) {
	validEntries := []string{
		`{statement: orderedquery.Ordered(table, "sessions", "scope", 0, 25)}`,
		`{statement: orderedquery.Ordered(table, "sessions", "scope", 500, 25)}`,
		`{statement: orderedquery.Ranked(table, "sessions", "rank", nil, 24)}`,
		`{statement: orderedquery.Ranked(table, "sessions", "rank", &orderedquery.RankedPosition{}, 24)}`,
		`{statement: orderedquery.Due(table, "sessions", 999, nil, 24)}`,
		`{statement: orderedquery.Due(table, "sessions", 999, &orderedquery.DuePosition{}, 24)}`,
	}
	copied := `orderedquery.Statement{SQL: "SELECT namespace, ordering_scope, stable_key, ranking_scope, revision::text, order_id::text, value, value_is_nil, ranked, rank_value, due_state, due_at, deleted FROM records WHERE namespace = $1 AND ranking_scope = $2 AND ranked AND NOT deleted ORDER BY rank_value DESC, stable_key DESC, ordering_scope DESC LIMIT $3"}`
	formattedCopy := "orderedquery.Statement{SQL: `SELECT namespace, ordering_scope, stable_key\nFROM records\nWHERE namespace = $1\n  AND due_state = 1\n  AND NOT deleted\nORDER BY due_at ASC, stable_key ASC, ordering_scope ASC\nLIMIT $3`}"

	tests := []struct {
		name    string
		entries []string
		extra   string
	}{
		{name: "exact copied ranked SQL", entries: replaceEntry(validEntries, 2, `{statement: `+copied+`}`)},
		{name: "format-varied copied due SQL", entries: replaceEntry(validEntries, 4, `{statement: `+formattedCopy+`}`)},
		{name: "copied SQL plus unused builder", entries: replaceEntry(validEntries, 0, `{statement: orderedquery.Statement{SQL: "SELECT " + orderedquery.RecordColumns + " FROM " + table + " WHERE namespace = $1 AND ordering_scope = $2 AND order_id > $3::numeric ORDER BY " + table + ".order_id ASC, stable_key ASC LIMIT $4"}}`), extra: `_ = orderedquery.Ordered(table, "sessions", "scope", 0, 25)`},
		{name: "duplicate first ranked page", entries: replaceEntry(validEntries, 3, `{statement: orderedquery.Ranked(table, "sessions", "rank", nil, 24)}`)},
		{name: "no plan table", entries: nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validatePlanGateStatementOwnership([]byte(planOwnershipFixture(test.entries, test.extra))); err == nil {
				t.Fatal("ownership validation accepted a copied, unused, duplicate, or vacuous plan fixture")
			}
		})
	}
}

func TestPlanGateStatementOwnershipAllowsLegalFormatting(t *testing.T) {
	entries := []string{
		`{statement: (orderedquery.Ordered(
			table, "sessions", "scope", (0), 25,
		))}`,
		`{statement: ((orderedquery.Ordered(table, "sessions", "scope", 500, 25)))}`,
		`{statement: orderedquery.Ranked(table, "sessions", "rank", (nil), 24)}`,
		`{statement: (orderedquery.Ranked(table, "sessions", "rank", &orderedquery.RankedPosition{}, 24))}`,
		`{statement: orderedquery.Due(table, "sessions", 999, nil, 24)}`,
		`{statement: orderedquery.Due(
			table, "sessions", 999,
			(&orderedquery.DuePosition{}), 24,
		)}`,
	}
	if err := validatePlanGateStatementOwnership([]byte(planOwnershipFixture(entries, ""))); err != nil {
		t.Fatalf("ownership validation rejected legal formatting: %v", err)
	}
}

func TestProductionStatementOwnershipRequiresConsumedDirectBuilders(t *testing.T) {
	valid := productionOwnershipFixture(
		`q := orderedquery.Ordered(table, namespace, scope, after, limit); rows, _ := s.pool.Query(ctx, q.SQL, q.Args...)`,
		`rankStatement := orderedquery.Ranked(table, namespace, scope, position, limit); rows, _ := s.pool.Query(ctx, rankStatement.SQL, rankStatement.Args...)`,
		`dueStatement := (orderedquery.Due(table, namespace, bound, position, limit)); rows, _ := s.pool.Query(ctx, dueStatement.SQL, dueStatement.Args...)`,
	)
	if err := validateProductionStatementOwnership([]byte(valid)); err != nil {
		t.Fatalf("production ownership rejected direct consumed builders: %v", err)
	}

	tests := []struct {
		name   string
		ranked string
	}{
		{name: "unused builder cannot satisfy ownership", ranked: `_ = orderedquery.Ranked(table, namespace, scope, position, limit); q := orderedquery.Statement{SQL: "SELECT copied"}; rows, _ := s.pool.Query(ctx, q.SQL, q.Args...)`},
		{name: "shadowed unused builder cannot satisfy ownership", ranked: `q := orderedquery.Statement{SQL: "SELECT copied"}; { q := orderedquery.Ranked(table, namespace, scope, position, limit); _ = q }; rows, _ := s.pool.Query(ctx, q.SQL, q.Args...)`},
		{name: "builder result not consumed", ranked: `q := orderedquery.Ranked(table, namespace, scope, position, limit); copied := orderedquery.Statement{SQL: "SELECT copied"}; rows, _ := s.pool.Query(ctx, copied.SQL, copied.Args...)`},
		{name: "wrong family", ranked: `q := orderedquery.Due(table, namespace, bound, position, limit); rows, _ := s.pool.Query(ctx, q.SQL, q.Args...)`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := productionOwnershipFixture(
				`q := orderedquery.Ordered(table, namespace, scope, after, limit); rows, _ := s.pool.Query(ctx, q.SQL, q.Args...)`,
				test.ranked,
				`q := orderedquery.Due(table, namespace, bound, position, limit); rows, _ := s.pool.Query(ctx, q.SQL, q.Args...)`,
			)
			if err := validateProductionStatementOwnership([]byte(source)); err == nil {
				t.Fatal("production ownership accepted an unused, unconsumed, or wrong-family builder")
			}
		})
	}
}

func replaceEntry(entries []string, index int, replacement string) []string {
	result := append([]string(nil), entries...)
	result[index] = replacement
	return result
}

func planOwnershipFixture(entries []string, extra string) string {
	return fmt.Sprintf(`package fixture
func planGate() {
	table := "records"
	%s
	_ = []struct { statement orderedquery.Statement }{
		%s,
	}
}`, extra, strings.Join(entries, ",\n"))
}

func productionOwnershipFixture(ordered, ranked, due string) string {
	return fmt.Sprintf(`package fixture
func (s *Store) ListOrdered() { %s }
func (s *Store) ListRanked() { %s }
func (s *Store) ListDue() { %s }
`, ordered, ranked, due)
}

type pageKinds struct {
	first  int
	middle int
}

func validatePlanGateStatementOwnership(source []byte) error {
	file, err := parser.ParseFile(token.NewFileSet(), "plan_gate.go", source, 0)
	if err != nil {
		return fmt.Errorf("parse plan gate: %w", err)
	}
	var tables []*ast.CompositeLit
	ast.Inspect(file, func(node ast.Node) bool {
		literal, ok := node.(*ast.CompositeLit)
		if ok && isPlanCaseTable(literal.Type) {
			tables = append(tables, literal)
		}
		return true
	})
	if len(tables) != 1 {
		return fmt.Errorf("found %d plan-case tables, want exactly 1", len(tables))
	}
	if len(tables[0].Elts) != 6 {
		return fmt.Errorf("plan-case table has %d cases, want exactly 6", len(tables[0].Elts))
	}

	pages := map[string]pageKinds{"Ordered": {}, "Ranked": {}, "Due": {}}
	for index, element := range tables[0].Elts {
		caseLiteral, ok := unparen(element).(*ast.CompositeLit)
		if !ok {
			return fmt.Errorf("plan case %d is not a keyed composite literal", index+1)
		}
		statement, err := keyedField(caseLiteral, "statement")
		if err != nil {
			return fmt.Errorf("plan case %d: %w", index+1, err)
		}
		family, call, ok := orderedQueryCall(statement)
		if !ok {
			return fmt.Errorf("plan case %d statement is not a direct orderedquery page-builder call", index+1)
		}
		if len(call.Args) != 5 {
			return fmt.Errorf("plan case %d %s call has %d arguments, want 5", index+1, family, len(call.Args))
		}
		kind := pages[family]
		switch family {
		case "Ordered":
			if isZeroInteger(call.Args[3]) {
				kind.first++
			} else if isNonzeroInteger(call.Args[3]) {
				kind.middle++
			} else {
				return fmt.Errorf("plan case %d Ordered cursor is not a literal first/middle position", index+1)
			}
		case "Ranked", "Due":
			if isNil(call.Args[3]) {
				kind.first++
			} else {
				kind.middle++
			}
		default:
			return fmt.Errorf("plan case %d uses unexpected orderedquery.%s builder", index+1, family)
		}
		pages[family] = kind
	}
	for _, family := range []string{"Ordered", "Ranked", "Due"} {
		if got := pages[family]; got.first != 1 || got.middle != 1 {
			return fmt.Errorf("orderedquery.%s plan coverage = %d first/%d middle, want 1/1", family, got.first, got.middle)
		}
	}
	return nil
}

func validateProductionStatementOwnership(source []byte) error {
	file, err := parser.ParseFile(token.NewFileSet(), "orderedindex.go", source, 0)
	if err != nil {
		return fmt.Errorf("parse OrderedIndex production: %w", err)
	}
	functions := make(map[string]*ast.FuncDecl)
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok {
			functions[function.Name.Name] = function
		}
	}
	for _, family := range []string{"Ordered", "Ranked", "Due"} {
		function := functions["List"+family]
		if function == nil || function.Body == nil {
			return fmt.Errorf("missing List%s implementation", family)
		}
		owned := false
		ast.Inspect(function.Body, func(node ast.Node) bool {
			assignment, ok := node.(*ast.AssignStmt)
			if !ok || len(assignment.Lhs) != len(assignment.Rhs) {
				return true
			}
			for index, right := range assignment.Rhs {
				builder, _, direct := orderedQueryCall(right)
				name, named := assignment.Lhs[index].(*ast.Ident)
				if direct && builder == family && named && queryConsumesStatement(function.Body, name) {
					owned = true
				}
			}
			return true
		})
		if !owned {
			return fmt.Errorf("List%s does not pass a directly built orderedquery.%s statement and args to Query", family, family)
		}
	}
	return nil
}

func isPlanCaseTable(expression ast.Expr) bool {
	array, ok := unparen(expression).(*ast.ArrayType)
	if !ok {
		return false
	}
	structure, ok := unparen(array.Elt).(*ast.StructType)
	if !ok {
		return false
	}
	for _, field := range structure.Fields.List {
		for _, name := range field.Names {
			if name.Name == "statement" && isOrderedQueryStatement(field.Type) {
				return true
			}
		}
	}
	return false
}

func isOrderedQueryStatement(expression ast.Expr) bool {
	selector, ok := unparen(expression).(*ast.SelectorExpr)
	packageName, packageOK := selectorPackage(selector)
	return ok && packageOK && packageName == "orderedquery" && selector.Sel.Name == "Statement"
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

func orderedQueryCall(expression ast.Expr) (string, *ast.CallExpr, bool) {
	call, ok := unparen(expression).(*ast.CallExpr)
	if !ok {
		return "", nil, false
	}
	selector, ok := unparen(call.Fun).(*ast.SelectorExpr)
	packageName, packageOK := selectorPackage(selector)
	if !ok || !packageOK || packageName != "orderedquery" {
		return "", nil, false
	}
	return selector.Sel.Name, call, true
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

func queryConsumesStatement(body *ast.BlockStmt, statement *ast.Ident) bool {
	consumed := false
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) < 3 || call.Ellipsis == token.NoPos {
			return true
		}
		selector, ok := unparen(call.Fun).(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Query" {
			return true
		}
		if isStatementSelector(call.Args[1], statement, "SQL") && isStatementSelector(call.Args[2], statement, "Args") {
			consumed = true
		}
		return true
	})
	return consumed
}

func isStatementSelector(expression ast.Expr, statement *ast.Ident, field string) bool {
	selector, ok := unparen(expression).(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != field {
		return false
	}
	identifier, ok := unparen(selector.X).(*ast.Ident)
	if !ok {
		return false
	}
	if statement.Obj != nil || identifier.Obj != nil {
		return statement.Obj != nil && statement.Obj == identifier.Obj
	}
	return statement == identifier
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

func TestOrderedIndexMigrationCarriesExactPhysicalIndexes(t *testing.T) {
	source, err := os.ReadFile("migrations/0003_ordered_index.sql")
	if err != nil {
		t.Fatal(err)
	}
	statement := string(source)
	for _, required := range []string{
		"stable_key bytea NOT NULL",
		"PRIMARY KEY (namespace, ordering_scope, stable_key)",
		"(namespace, ordering_scope, order_id, stable_key)",
		"(namespace, ranking_scope, rank_value DESC, stable_key DESC, ordering_scope DESC)",
		"(namespace, due_state, due_at, stable_key, ordering_scope)",
	} {
		if !strings.Contains(statement, required) {
			t.Errorf("OrderedIndex migration lost exact index %q", required)
		}
	}
	if strings.Contains(statement, `stable_key text`) {
		t.Fatal("OrderedIndex migration cannot represent embedded-NUL StableKeys in PostgreSQL text")
	}
	if strings.Contains(statement, "due_state, ordering_scope") {
		t.Fatal("due index invented a scope filter absent from Storage v0.6.0 ListDue")
	}
}

func TestOrderedIndexMutationsPinReadCommittedAndExplicitRowLocks(t *testing.T) {
	source, err := os.ReadFile("internal/orderedindex/orderedindex.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	if got := strings.Count(text, "IsoLevel: pgx.ReadCommitted"); got != 3 {
		t.Fatalf("OrderedIndex explicit Read Committed transaction count = %d, want 3", got)
	}
	if strings.Contains(text, "pgx.Serializable") {
		t.Fatal("OrderedIndex uses Serializable despite its per-scope and per-row lock protocol")
	}
	if !strings.Contains(text, "WHERE namespace = $1 AND ordering_scope = $2 FOR UPDATE") {
		t.Fatal("OrderedIndex Create lost its per-scope counter row lock")
	}
	if !strings.Contains(text, `return s.getFrom(ctx, tx, id, " FOR UPDATE")`) {
		t.Fatal("OrderedIndex Update/Delete lost their shared authoritative-row lock")
	}
}
