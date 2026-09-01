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
		`{family: orderedPlanOrder, page: orderedPlanFirst, statement: orderedquery.Ordered(table, "sessions", "scope", 0, 25)}`,
		`{family: orderedPlanOrder, page: orderedPlanMiddle, statement: orderedquery.Ordered(table, "sessions", "scope", 500, 25)}`,
		`{family: orderedPlanRanked, page: orderedPlanFirst, statement: orderedquery.Ranked(table, "sessions", "rank", nil, 24)}`,
		`{family: orderedPlanRanked, page: orderedPlanMiddle, statement: orderedquery.Ranked(table, "sessions", "rank", &orderedquery.RankedPosition{}, 24)}`,
		`{family: orderedPlanDue, page: orderedPlanFirst, statement: orderedquery.Due(table, "sessions", 999, nil, 24)}`,
		`{family: orderedPlanDue, page: orderedPlanMiddle, statement: orderedquery.Due(table, "sessions", 999, &orderedquery.DuePosition{}, 24)}`,
	}
	copied := `orderedquery.Statement{SQL: "SELECT namespace, ordering_scope, stable_key, ranking_scope, revision::text, order_id::text, value, value_is_nil, ranked, rank_value, due_state, due_at, deleted FROM records WHERE namespace = $1 AND ranking_scope = $2 AND ranked AND NOT deleted ORDER BY rank_value DESC, stable_key DESC, ordering_scope DESC LIMIT $3"}`
	formattedCopy := "orderedquery.Statement{SQL: `SELECT namespace, ordering_scope, stable_key\nFROM records\nWHERE namespace = $1\n  AND due_state = 1\n  AND NOT deleted\nORDER BY due_at ASC, stable_key ASC, ordering_scope ASC\nLIMIT $3`}"

	tests := []struct {
		name    string
		entries []string
		extra   string
	}{
		{name: "exact copied ranked SQL", entries: replaceEntry(validEntries, 2, `{family: orderedPlanRanked, page: orderedPlanFirst, statement: `+copied+`}`)},
		{name: "format-varied copied due SQL", entries: replaceEntry(validEntries, 4, `{family: orderedPlanDue, page: orderedPlanFirst, statement: `+formattedCopy+`}`)},
		{name: "copied SQL plus unused builder", entries: replaceEntry(validEntries, 0, `{family: orderedPlanOrder, page: orderedPlanFirst, statement: orderedquery.Statement{SQL: "SELECT " + orderedquery.RecordColumns + " FROM " + table + " WHERE namespace = $1 AND ordering_scope = $2 AND order_id > $3::numeric ORDER BY " + table + ".order_id ASC, stable_key ASC LIMIT $4"}}`), extra: `_ = orderedquery.Ordered(table, "sessions", "scope", 0, 25)`},
		{name: "duplicate first ranked page", entries: replaceEntry(validEntries, 3, `{family: orderedPlanRanked, page: orderedPlanMiddle, statement: orderedquery.Ranked(table, "sessions", "rank", nil, 24)}`)},
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
		`{family: (orderedPlanOrder), page: orderedPlanFirst, statement: (orderedquery.Ordered(
			table, "sessions", "scope", (0), 25,
		))}`,
		`{family: orderedPlanOrder, page: (orderedPlanMiddle), statement: ((orderedquery.Ordered(table, "sessions", "scope", 500, 25)))}`,
		`{family: orderedPlanRanked, page: orderedPlanFirst, statement: orderedquery.Ranked(table, "sessions", "rank", ((*orderedquery.RankedPosition)(nil)), 24)}`,
		`{family: orderedPlanRanked, page: orderedPlanMiddle, statement: (orderedquery.Ranked(table, "sessions", "rank", &orderedquery.RankedPosition{}, 24))}`,
		`{family: orderedPlanDue, page: orderedPlanFirst, statement: orderedquery.Due(table, "sessions", 999, (nil), 24)}`,
		`{family: orderedPlanDue, page: orderedPlanMiddle, statement: orderedquery.Due(
			table, "sessions", 999,
			(&orderedquery.DuePosition{}), 24,
		)}`,
	}
	legalLoop := `for _, test := range (orderedPlanCases(
		table, prefix,
	)) {
		(tx).QueryRow(ctx, ("EXPLAIN "+((test.statement.SQL))), ((test.statement.Args))...)
	}`
	if err := validatePlanGateStatementOwnership([]byte(planOwnershipFixtureWithLoop(entries, "", legalLoop))); err != nil {
		t.Fatalf("ownership validation rejected legal formatting: %v", err)
	}
}

func TestPlanGateOwnershipBindsTheActuallyExplainedCaseSource(t *testing.T) {
	deadEntries := []string{
		`{family: orderedPlanOrder, page: orderedPlanFirst, statement: orderedquery.Ordered(table, "sessions", "scope", 0, 25)}`,
		`{family: orderedPlanOrder, page: orderedPlanMiddle, statement: orderedquery.Ordered(table, "sessions", "scope", 500, 25)}`,
		`{family: orderedPlanRanked, page: orderedPlanFirst, statement: orderedquery.Ranked(table, "sessions", "rank", nil, 24)}`,
		`{family: orderedPlanRanked, page: orderedPlanMiddle, statement: orderedquery.Ranked(table, "sessions", "rank", &orderedquery.RankedPosition{}, 24)}`,
		`{family: orderedPlanDue, page: orderedPlanFirst, statement: orderedquery.Due(table, "sessions", 999, nil, 24)}`,
		`{family: orderedPlanDue, page: orderedPlanMiddle, statement: orderedquery.Due(table, "sessions", 999, &orderedquery.DuePosition{}, 24)}`,
	}
	t.Run("dead helper and live copied table", func(t *testing.T) {
		deadTableLiveCopy := planOwnershipFixtureWithLoop(deadEntries, "", `live := []orderedPlanCase{{statement: orderedquery.Statement{SQL: "SELECT copied"}}}
	for _, test := range live { tx.QueryRow(ctx, "EXPLAIN "+test.statement.SQL, test.statement.Args...) }`)
		if err := validatePlanGateStatementOwnership([]byte(deadTableLiveCopy)); err == nil {
			t.Fatal("ownership validation accepted a dead valid helper while the live EXPLAIN loop consumed copied SQL")
		}
	})
	t.Run("typed nil marked middle", func(t *testing.T) {
		typedNilMiddle := replaceEntry(deadEntries, 3, `{family: orderedPlanRanked, page: orderedPlanMiddle, statement: orderedquery.Ranked(table, "sessions", "rank", (*orderedquery.RankedPosition)(nil), 24)}`)
		if err := validatePlanGateStatementOwnership([]byte(planOwnershipFixture(typedNilMiddle, ""))); err == nil {
			t.Fatal("ownership validation classified a typed nil ranked cursor as a middle page")
		}
	})
}

func TestProductionStatementOwnershipRequiresConsumedDirectBuilders(t *testing.T) {
	valid := productionOwnershipFixture(
		`q := orderedquery.Ordered(table, namespace, scope, after, limit); rows, _ := ((s.pool.Query))(ctx, (q.SQL), (q.Args)...)`,
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

func TestProductionOwnershipRejectsDecoyQueriesAndReceiverConfusion(t *testing.T) {
	ordered := `q := orderedquery.Ordered(table, namespace, scope, after, limit); rows, _ := s.pool.Query(ctx, q.SQL, q.Args...)`
	due := `q := orderedquery.Due(table, namespace, bound, position, limit); rows, _ := s.pool.Query(ctx, q.SQL, q.Args...)`
	tests := []struct {
		name   string
		ranked string
	}{
		{name: "dead second receiver query", ranked: `q := orderedquery.Ranked(table, namespace, scope, position, limit); if false { s.pool.Query(ctx, "SELECT decoy") }; rows, _ := s.pool.Query(ctx, q.SQL, q.Args...)`},
		{name: "second query on another receiver", ranked: `q := orderedquery.Ranked(table, namespace, scope, position, limit); if false { other.pool.Query(ctx, "SELECT decoy") }; rows, _ := s.pool.Query(ctx, q.SQL, q.Args...)`},
		{name: "copied live query with dead valid query", ranked: `valid := orderedquery.Ranked(table, namespace, scope, position, limit); if false { s.pool.Query(ctx, valid.SQL, valid.Args...) }; copied := orderedquery.Statement{SQL: "SELECT copied"}; rows, _ := s.pool.Query(ctx, copied.SQL, copied.Args...)`},
		{name: "receiver shadow", ranked: `q := orderedquery.Ranked(table, namespace, scope, position, limit); { s := other; rows, _ := s.pool.Query(ctx, q.SQL, q.Args...); _ = rows }`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateProductionStatementOwnership([]byte(productionOwnershipFixture(ordered, test.ranked, due))); err == nil {
				t.Fatal("production ownership accepted a decoy, copied live query, or receiver shadow")
			}
		})
	}

	// Duplicate method declarations are parser-valid, but intentionally invalid to
	// the Go type checker. The structural guard must diagnose the duplicate rather
	// than silently letting a bare-name map overwrite one declaration.
	duplicate := productionOwnershipFixture(ordered, `copied := orderedquery.Statement{SQL: "SELECT copied"}; rows, _ := s.pool.Query(ctx, copied.SQL, copied.Args...)`, due) +
		`func (s *Store) ListRanked() { q := orderedquery.Ranked(table, namespace, scope, position, limit); rows, _ := s.pool.Query(ctx, q.SQL, q.Args...) }`
	if _, err := parser.ParseFile(token.NewFileSet(), "duplicate.go", duplicate, 0); err != nil {
		t.Fatalf("duplicate declaration fixture is not parser-valid: %v", err)
	}
	if err := validateProductionStatementOwnership([]byte(duplicate)); err == nil {
		t.Fatal("production ownership accepted duplicate ListRanked method declarations")
	}
}

func replaceEntry(entries []string, index int, replacement string) []string {
	result := append([]string(nil), entries...)
	result[index] = replacement
	return result
}

func planOwnershipFixture(entries []string, extra string) string {
	return planOwnershipFixtureWithLoop(entries, extra, `for _, test := range orderedPlanCases(table, prefix) {
		tx.QueryRow(ctx, "EXPLAIN "+test.statement.SQL, test.statement.Args...)
	}`)
}

func planOwnershipFixtureWithLoop(entries []string, extra, loop string) string {
	return fmt.Sprintf(`package fixture
type orderedPlanFamily string
const ( orderedPlanOrder orderedPlanFamily = "order"; orderedPlanRanked orderedPlanFamily = "ranked"; orderedPlanDue orderedPlanFamily = "due" )
type orderedPlanPage string
const ( orderedPlanFirst orderedPlanPage = "first"; orderedPlanMiddle orderedPlanPage = "middle" )
type orderedPlanCase struct { family orderedPlanFamily; page orderedPlanPage; statement orderedquery.Statement }
func orderedPlanCases(table, prefix string) []orderedPlanCase {
	%s
	return []orderedPlanCase{
		%s,
	}
}
func TestOrderedIndexPlansUseExactKeysetIndexesForFirstAndMiddlePages() {
	table := "records"
	prefix := "p"
	tx, _ := admin.BeginTx(ctx)
	%s
}`, extra, strings.Join(entries, ",\n"), loop)
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
	helpers := freeFunctionsNamed(file, "orderedPlanCases")
	if len(helpers) != 1 {
		return fmt.Errorf("found %d orderedPlanCases helpers, want exactly 1", len(helpers))
	}
	table, err := returnedPlanCaseTable(helpers[0])
	if err != nil {
		return err
	}
	if len(table.Elts) != 6 {
		return fmt.Errorf("orderedPlanCases has %d cases, want exactly 6", len(table.Elts))
	}

	pages := map[string]pageKinds{"Ordered": {}, "Ranked": {}, "Due": {}}
	for index, element := range table.Elts {
		caseLiteral, ok := unparen(element).(*ast.CompositeLit)
		if !ok {
			return fmt.Errorf("plan case %d is not a keyed composite literal", index+1)
		}
		familyMetadata, err := keyedIdentifier(caseLiteral, "family")
		if err != nil {
			return fmt.Errorf("plan case %d: %w", index+1, err)
		}
		pageMetadata, err := keyedIdentifier(caseLiteral, "page")
		if err != nil {
			return fmt.Errorf("plan case %d: %w", index+1, err)
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
		wantFamilyMetadata := map[string]string{
			"Ordered": "orderedPlanOrder",
			"Ranked":  "orderedPlanRanked",
			"Due":     "orderedPlanDue",
		}[family]
		if wantFamilyMetadata == "" || familyMetadata != wantFamilyMetadata {
			return fmt.Errorf("plan case %d family metadata %s disagrees with orderedquery.%s", index+1, familyMetadata, family)
		}
		kind := pages[family]
		actualPage := ""
		switch family {
		case "Ordered":
			if isZeroInteger(call.Args[3]) {
				actualPage = "orderedPlanFirst"
			} else if isNonzeroInteger(call.Args[3]) {
				actualPage = "orderedPlanMiddle"
			} else {
				return fmt.Errorf("plan case %d Ordered cursor is not a literal first/middle position", index+1)
			}
		case "Ranked", "Due":
			if isNilCursor(call.Args[3], family) {
				actualPage = "orderedPlanFirst"
			} else {
				actualPage = "orderedPlanMiddle"
			}
		default:
			return fmt.Errorf("plan case %d uses unexpected orderedquery.%s builder", index+1, family)
		}
		if pageMetadata != actualPage {
			return fmt.Errorf("plan case %d page metadata %s disagrees with its %s cursor", index+1, pageMetadata, actualPage)
		}
		if actualPage == "orderedPlanFirst" {
			kind.first++
		} else {
			kind.middle++
		}
		pages[family] = kind
	}
	for _, family := range []string{"Ordered", "Ranked", "Due"} {
		if got := pages[family]; got.first != 1 || got.middle != 1 {
			return fmt.Errorf("orderedquery.%s plan coverage = %d first/%d middle, want 1/1", family, got.first, got.middle)
		}
	}
	return validatePlanIntegrationLoop(file)
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

func returnedPlanCaseTable(function *ast.FuncDecl) (*ast.CompositeLit, error) {
	var returns []*ast.ReturnStmt
	ast.Inspect(function.Body, func(node ast.Node) bool {
		if statement, ok := node.(*ast.ReturnStmt); ok {
			returns = append(returns, statement)
		}
		return true
	})
	if len(returns) != 1 || len(returns[0].Results) != 1 {
		return nil, fmt.Errorf("orderedPlanCases must have exactly one single-value return")
	}
	literal, ok := unparen(returns[0].Results[0]).(*ast.CompositeLit)
	if !ok || !isSliceOfNamed(literal.Type, "orderedPlanCase") {
		return nil, fmt.Errorf("orderedPlanCases must directly return []orderedPlanCase")
	}
	return literal, nil
}

func isSliceOfNamed(expression ast.Expr, name string) bool {
	array, ok := unparen(expression).(*ast.ArrayType)
	if !ok || array.Len != nil {
		return false
	}
	identifier, ok := unparen(array.Elt).(*ast.Ident)
	return ok && identifier.Name == name
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

func validatePlanIntegrationLoop(file *ast.File) error {
	tests := freeFunctionsNamed(file, "TestOrderedIndexPlansUseExactKeysetIndexesForFirstAndMiddlePages")
	if len(tests) != 1 {
		return fmt.Errorf("found %d exact ordered-plan integration tests, want exactly 1", len(tests))
	}
	test := tests[0]
	var transactions []*ast.Ident
	ast.Inspect(test.Body, func(node ast.Node) bool {
		assignment, ok := node.(*ast.AssignStmt)
		if !ok || len(assignment.Lhs) == 0 || len(assignment.Rhs) == 0 || !isSelectorCall(assignment.Rhs[0], "BeginTx") {
			return true
		}
		if identifier, ok := assignment.Lhs[0].(*ast.Ident); ok {
			transactions = append(transactions, identifier)
		}
		return true
	})
	if len(transactions) != 1 {
		return fmt.Errorf("plan integration test has %d BeginTx result bindings, want exactly 1", len(transactions))
	}
	var ranges []*ast.RangeStmt
	ast.Inspect(test.Body, func(node ast.Node) bool {
		rangeStatement, ok := node.(*ast.RangeStmt)
		if !ok || !isIdentifierCall(rangeStatement.X, "orderedPlanCases") {
			return true
		}
		ranges = append(ranges, rangeStatement)
		return true
	})
	if len(ranges) != 1 {
		return fmt.Errorf("plan integration test has %d direct orderedPlanCases ranges, want exactly 1", len(ranges))
	}
	rangeValue, ok := ranges[0].Value.(*ast.Ident)
	if !ok || rangeValue.Name == "_" {
		return fmt.Errorf("orderedPlanCases range must bind its case value")
	}
	var queryRows []*ast.CallExpr
	ast.Inspect(test.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := unparen(call.Fun).(*ast.SelectorExpr)
		if ok && selector.Sel.Name == "QueryRow" {
			queryRows = append(queryRows, call)
		}
		return true
	})
	if len(queryRows) != 1 {
		return fmt.Errorf("plan integration test has %d QueryRow calls, want exactly 1", len(queryRows))
	}
	query := queryRows[0]
	if !nodeContains(ranges[0].Body, query) {
		return fmt.Errorf("plan integration QueryRow is not inside the orderedPlanCases range")
	}
	selector := unparen(query.Fun).(*ast.SelectorExpr)
	queryReceiver, ok := unparen(selector.X).(*ast.Ident)
	if !ok || !sameObject(queryReceiver, transactions[0]) {
		return fmt.Errorf("plan integration QueryRow is not called on its BeginTx result")
	}
	if len(query.Args) != 3 || query.Ellipsis == token.NoPos || !isExplainStatement(query.Args[1], rangeValue) || !isNestedSelector(query.Args[2], rangeValue, "statement", "Args") {
		return fmt.Errorf("plan integration QueryRow must explain the ranged case statement SQL with its variadic Args")
	}
	return nil
}

func isSelectorCall(expression ast.Expr, method string) bool {
	call, ok := unparen(expression).(*ast.CallExpr)
	if !ok {
		return false
	}
	selector, ok := unparen(call.Fun).(*ast.SelectorExpr)
	return ok && selector.Sel.Name == method
}

func isIdentifierCall(expression ast.Expr, name string) bool {
	call, ok := unparen(expression).(*ast.CallExpr)
	if !ok {
		return false
	}
	identifier, ok := unparen(call.Fun).(*ast.Ident)
	return ok && identifier.Name == name
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

func isExplainStatement(expression ast.Expr, value *ast.Ident) bool {
	binary, ok := unparen(expression).(*ast.BinaryExpr)
	if !ok || binary.Op != token.ADD {
		return false
	}
	literal, ok := unparen(binary.X).(*ast.BasicLit)
	return ok && literal.Kind == token.STRING && strings.Contains(literal.Value, "EXPLAIN") && isNestedSelector(binary.Y, value, "statement", "SQL")
}

func isNestedSelector(expression ast.Expr, root *ast.Ident, first, second string) bool {
	outer, ok := unparen(expression).(*ast.SelectorExpr)
	if !ok || outer.Sel.Name != second {
		return false
	}
	inner, ok := unparen(outer.X).(*ast.SelectorExpr)
	if !ok || inner.Sel.Name != first {
		return false
	}
	identifier, ok := unparen(inner.X).(*ast.Ident)
	return ok && sameObject(identifier, root)
}

func validateProductionStatementOwnership(source []byte) error {
	file, err := parser.ParseFile(token.NewFileSet(), "orderedindex.go", source, 0)
	if err != nil {
		return fmt.Errorf("parse OrderedIndex production: %w", err)
	}
	for _, family := range []string{"Ordered", "Ranked", "Due"} {
		methods := storeMethodsNamed(file, "List"+family)
		if len(methods) != 1 {
			return fmt.Errorf("found %d *Store.List%s methods, want exactly 1", len(methods), family)
		}
		function, receiver := methods[0].function, methods[0].receiver
		var statements []*ast.Ident
		ast.Inspect(function.Body, func(node ast.Node) bool {
			assignment, ok := node.(*ast.AssignStmt)
			if !ok || len(assignment.Lhs) != len(assignment.Rhs) {
				return true
			}
			for index, right := range assignment.Rhs {
				builder, _, direct := orderedQueryCall(right)
				name, named := assignment.Lhs[index].(*ast.Ident)
				if direct && builder == family && named {
					statements = append(statements, name)
				}
			}
			return true
		})
		if len(statements) != 1 {
			return fmt.Errorf("List%s has %d direct orderedquery.%s statement assignments, want exactly 1", family, len(statements), family)
		}
		queries := methodCallsNamed(function.Body, "Query")
		if len(queries) != 1 {
			return fmt.Errorf("List%s has %d Query calls, want exactly 1", family, len(queries))
		}
		query := queries[0]
		if !isReceiverPoolCall(query, receiver) || len(query.Args) != 3 || query.Ellipsis == token.NoPos ||
			!isStatementSelector(query.Args[1], statements[0], "SQL") ||
			!isStatementSelector(query.Args[2], statements[0], "Args") {
			return fmt.Errorf("List%s Query is not its receiver-bound pool consuming the one orderedquery.%s statement SQL and variadic Args", family, family)
		}
	}
	return nil
}

type storeMethod struct {
	function *ast.FuncDecl
	receiver *ast.Ident
}

func storeMethodsNamed(file *ast.File, name string) []storeMethod {
	var methods []storeMethod
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != name || function.Body == nil || function.Recv == nil || len(function.Recv.List) != 1 {
			continue
		}
		receiverField := function.Recv.List[0]
		if len(receiverField.Names) != 1 || !isPointerToNamed(receiverField.Type, "Store") {
			continue
		}
		methods = append(methods, storeMethod{function: function, receiver: receiverField.Names[0]})
	}
	return methods
}

func isPointerToNamed(expression ast.Expr, name string) bool {
	pointer, ok := unparen(expression).(*ast.StarExpr)
	if !ok {
		return false
	}
	identifier, ok := unparen(pointer.X).(*ast.Ident)
	return ok && identifier.Name == name
}

func methodCallsNamed(body *ast.BlockStmt, method string) []*ast.CallExpr {
	var calls []*ast.CallExpr
	ast.Inspect(body, func(node ast.Node) bool {
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

func isReceiverPoolCall(call *ast.CallExpr, receiver *ast.Ident) bool {
	methodSelector, ok := unparen(call.Fun).(*ast.SelectorExpr)
	if !ok || methodSelector.Sel.Name != "Query" {
		return false
	}
	poolSelector, ok := unparen(methodSelector.X).(*ast.SelectorExpr)
	if !ok || poolSelector.Sel.Name != "pool" {
		return false
	}
	candidate, ok := unparen(poolSelector.X).(*ast.Ident)
	return ok && sameObject(candidate, receiver)
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

func isNilCursor(expression ast.Expr, family string) bool {
	if isNil(expression) {
		return true
	}
	conversion, ok := unparen(expression).(*ast.CallExpr)
	if !ok || len(conversion.Args) != 1 || !isNil(conversion.Args[0]) {
		return false
	}
	pointer, ok := unparen(conversion.Fun).(*ast.StarExpr)
	if !ok {
		return false
	}
	selector, ok := unparen(pointer.X).(*ast.SelectorExpr)
	packageName, packageOK := selectorPackage(selector)
	return ok && packageOK && packageName == "orderedquery" && selector.Sel.Name == family+"Position"
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
