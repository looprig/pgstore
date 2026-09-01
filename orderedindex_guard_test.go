package pgstore

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/looprig/pgstore/internal/orderedquery"
)

// The four OrderedIndex prohibitions — no OFFSET, no in-memory global sort, no
// table scan, no fallback to another primitive — are classified by functions
// over the parsed code, not by strings.Contains over the raw file. Substrings
// were evadable in three of the four ways measured: "\nOFFSET 0" is not
// " offset ", slices.SortFunc is not "sort.", a table-level fallback is not
// "internal/kv", and a required ORDER BY fragment kept alive as a comment
// satisfied a raw-source match while the live SQL had none.

var orderedProductionPaths = []string{
	"internal/orderedindex/orderedindex.go",
	"internal/orderedquery/orderedquery.go",
}

func parseOrderedProduction(t *testing.T) []*ast.File {
	t.Helper()
	files := make([]*ast.File, 0, len(orderedProductionPaths))
	for _, path := range orderedProductionPaths {
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		files = append(files, file)
	}
	return files
}

// orderedProductionStringLiterals is the only place SQL that reaches PostgreSQL
// can live. Reading literals rather than raw source means text preserved in a
// comment no longer satisfies a required fragment, and no longer hides a
// forbidden one behind a line break.
func orderedProductionStringLiterals(t *testing.T) []string {
	t.Helper()
	var literals []string
	for _, file := range parseOrderedProduction(t) {
		ast.Inspect(file, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Errorf("unquote OrderedIndex string literal %s: %v", literal.Value, err)
				return true
			}
			literals = append(literals, value)
			return true
		})
	}
	if len(literals) == 0 {
		t.Fatal("no OrderedIndex string literals found; the literal derivation no longer matches production")
	}
	return literals
}

func TestOrderedIndexQueriesRemainIndexBackedKeysets(t *testing.T) {
	t.Parallel()
	joined := strings.Join(orderedProductionStringLiterals(t), "\n")
	for _, required := range []string{
		"order_id > $3::numeric",
		"(rank_value, stable_key, ordering_scope) < ($3, $4, $5)",
		"(due_at, stable_key, ordering_scope) > ($3, $4, $5)",
		"ORDER BY rank_value DESC, stable_key DESC, ordering_scope DESC",
		"ORDER BY due_at ASC, stable_key ASC, ordering_scope ASC",
	} {
		if !strings.Contains(joined, required) {
			t.Errorf("OrderedIndex string literals lost required keyset fragment %q", required)
		}
	}
}

var offsetKeyword = regexp.MustCompile(`(?i)\boffset\b`)

func TestOrderedIndexStatementsNeverSkipRowsWithOffset(t *testing.T) {
	t.Parallel()
	for _, literal := range orderedProductionStringLiterals(t) {
		if offsetKeyword.MatchString(literal) {
			t.Errorf("OrderedIndex statement literal %q uses OFFSET; a keyset page must bound its start, not count past rows", literal)
		}
	}
}

func TestOffsetClassifierReadsTheKeywordNotTheSpacing(t *testing.T) {
	t.Parallel()
	for _, forbidden := range []string{"LIMIT $4 OFFSET 0", "LIMIT $4\nOFFSET 0", "limit $4\toffset 0", "LIMIT $4\nOFFSET\n0"} {
		if !offsetKeyword.MatchString(forbidden) {
			t.Errorf("OFFSET classifier missed %q; the prohibition is on the keyword, not on one spelling of the whitespace around it", forbidden)
		}
	}
	for _, allowed := range []string{"ORDER BY due_at ASC LIMIT $3", "SELECT offset_column FROM t", "SELECT byte_offset FROM t"} {
		if offsetKeyword.MatchString(allowed) {
			t.Errorf("OFFSET classifier rejected legal statement %q", allowed)
		}
	}
}

func TestOrderedIndexNeverSortsPagesInMemory(t *testing.T) {
	t.Parallel()
	for index, file := range parseOrderedProduction(t) {
		path := orderedProductionPaths[index]
		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("unquote import in %s: %v", path, err)
			}
			if importPath == "sort" || importPath == "slices" {
				t.Errorf("%s imports %q; a page's order is the index's order and must not be re-established in the process", path, importPath)
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := callSelectorName(call)
			if name == "" {
				if identifier, ok := unparen(call.Fun).(*ast.Ident); ok {
					name = identifier.Name
				}
			}
			if strings.Contains(strings.ToLower(name), "sort") {
				t.Errorf("%s calls %s; sorting records the database returned is a global in-memory sort however it is spelled", path, name)
			}
			return true
		})
	}
}

// migrationTableSuffixes derives every primitive's table name from the embedded
// migrations, so a new primitive is denied without anyone extending a list.
func migrationTableSuffixes(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	placeholder := regexp.MustCompile(`\{\{([a-z_]+)\}\}`)
	seen := make(map[string]bool)
	var suffixes []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		source, err := os.ReadFile(filepath.Join("migrations", entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		for _, match := range placeholder.FindAllStringSubmatch(string(source), -1) {
			if match[1] == "schema" || seen[match[1]] {
				continue
			}
			seen[match[1]] = true
			suffixes = append(suffixes, match[1])
		}
	}
	if len(suffixes) == 0 {
		t.Fatal("no migration table placeholders found; the suffix derivation no longer matches the migrations")
	}
	return suffixes
}

func TestOrderedIndexNeverFallsBackToAnotherPrimitive(t *testing.T) {
	t.Parallel()
	allowedInternal := map[string]bool{"guard": true, "orderedquery": true, "postgres": true}
	for index, file := range parseOrderedProduction(t) {
		path := orderedProductionPaths[index]
		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("unquote import in %s: %v", path, err)
			}
			const prefix = "github.com/looprig/pgstore/internal/"
			if !strings.HasPrefix(importPath, prefix) {
				continue
			}
			if name := strings.TrimPrefix(importPath, prefix); !allowedInternal[name] {
				t.Errorf("%s imports sibling primitive package %q; OrderedIndex has no fallback store", path, importPath)
			}
		}
	}

	// A package-path denial does not reach a fallback written at the table
	// level, which needs no import at all: the ordered statements may name only
	// the ordered tables.
	var forbidden []string
	for _, suffix := range migrationTableSuffixes(t) {
		if !strings.HasPrefix(suffix, "ordered_") {
			forbidden = append(forbidden, suffix)
		}
	}
	if len(forbidden) == 0 {
		t.Fatal("no non-ordered table suffixes derived; the fallback guard has no input")
	}
	literals := orderedProductionStringLiterals(t)
	for _, suffix := range forbidden {
		pattern := regexp.MustCompile(`\b` + regexp.QuoteMeta(suffix) + `\b`)
		for _, literal := range literals {
			if pattern.MatchString(literal) {
				t.Errorf("OrderedIndex literal %q names the %q table of another primitive", literal, suffix)
			}
		}
	}
}

type indexColumn struct {
	name       string
	descending bool
}

type declaredIndex struct {
	name      string
	columns   []indexColumn
	predicate []string
}

func balancedParenthesis(text string, open int) (string, int) {
	depth := 0
	for i := open; i < len(text); i++ {
		switch text[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return text[open+1 : i], i
			}
		}
	}
	return "", -1
}

func parseIndexColumns(list string) []indexColumn {
	var columns []indexColumn
	for _, item := range strings.Split(list, ",") {
		fields := strings.Fields(item)
		if len(fields) == 0 {
			continue
		}
		name := fields[0]
		if dot := strings.LastIndex(name, "."); dot >= 0 {
			name = name[dot+1:]
		}
		columns = append(columns, indexColumn{
			name:       strings.Trim(name, `"`),
			descending: len(fields) > 1 && strings.EqualFold(fields[1], "DESC"),
		})
	}
	return columns
}

var (
	createIndexHeader = regexp.MustCompile(`(?is)CREATE\s+INDEX\s+([a-z_0-9]+)\s+ON\s+[^(]+`)
	commentLine       = regexp.MustCompile(`(?m)--.*$`)
	equalityPredicate = regexp.MustCompile(`([a-z_]+)\s*=\s*(\$\d+|\d+)\b`)
)

// declaredOrderedIndexes reads the physical access paths the ordered migration
// actually creates. Deriving them means the coverage check below measures the
// statements against the schema rather than against a copy of it.
func declaredOrderedIndexes(t *testing.T) []declaredIndex {
	t.Helper()
	raw, err := os.ReadFile("migrations/0003_ordered_index.sql")
	if err != nil {
		t.Fatal(err)
	}
	source := commentLine.ReplaceAllString(strings.NewReplacer("{{", "", "}}", "").Replace(string(raw)), "")
	var indexes []declaredIndex
	for _, statement := range strings.Split(source, ";") {
		statement = strings.TrimSpace(statement)
		switch {
		case strings.HasPrefix(strings.ToUpper(statement), "CREATE TABLE"):
			key := strings.Index(statement, "PRIMARY KEY")
			if key < 0 {
				continue
			}
			open := strings.Index(statement[key:], "(")
			list, _ := balancedParenthesis(statement[key:], open)
			indexes = append(indexes, declaredIndex{name: "primary key", columns: parseIndexColumns(list)})
		case strings.HasPrefix(strings.ToUpper(statement), "CREATE INDEX"):
			header := createIndexHeader.FindStringSubmatchIndex(statement)
			if header == nil {
				t.Fatalf("unreadable CREATE INDEX statement: %s", statement)
			}
			list, end := balancedParenthesis(statement, header[1])
			if end < 0 {
				t.Fatalf("unbalanced column list: %s", statement)
			}
			index := declaredIndex{name: statement[header[2]:header[3]], columns: parseIndexColumns(list)}
			if where := strings.Index(strings.ToUpper(statement[end:]), "WHERE "); where >= 0 {
				for _, conjunct := range strings.Split(statement[end+where+len("WHERE "):], " AND ") {
					index.predicate = append(index.predicate, strings.Join(strings.Fields(conjunct), " "))
				}
			}
			indexes = append(indexes, index)
		}
	}
	if len(indexes) < 4 {
		t.Fatalf("derived %d ordered indexes, want the primary key and three secondary indexes", len(indexes))
	}
	return indexes
}

// statementAccessPath is the shape a page statement asks the planner for: the
// columns pinned to a constant, the residual predicate, and the total order.
type statementAccessPath struct {
	equality map[string]bool
	where    string
	orderBy  []indexColumn
}

func readAccessPath(sql string) (statementAccessPath, error) {
	order := strings.Index(sql, " ORDER BY ")
	if order < 0 {
		return statementAccessPath{}, fmt.Errorf("statement has no ORDER BY, so its page order is not the database's")
	}
	where := strings.Index(sql, " WHERE ")
	if where < 0 || where > order {
		return statementAccessPath{}, fmt.Errorf("statement has no WHERE, so it reads the whole table")
	}
	predicate := strings.Join(strings.Fields(sql[where+len(" WHERE "):order]), " ")
	trailing := sql[order+len(" ORDER BY "):]
	if limit := strings.Index(trailing, " LIMIT "); limit >= 0 {
		trailing = trailing[:limit]
	}
	path := statementAccessPath{equality: make(map[string]bool), where: predicate, orderBy: parseIndexColumns(trailing)}
	for _, match := range equalityPredicate.FindAllStringSubmatch(predicate, -1) {
		path.equality[match[1]] = true
	}
	if len(path.orderBy) == 0 {
		return statementAccessPath{}, fmt.Errorf("statement has an empty ORDER BY")
	}
	return path, nil
}

// coveringIndex reports the declared index that answers the statement without a
// sort: every leading index column the statement does not pin to a constant
// must be exactly the next ordering column, in the same direction.
func coveringIndex(indexes []declaredIndex, path statementAccessPath) (string, bool) {
	for _, index := range indexes {
		if !indexAnswers(index, path) {
			continue
		}
		return index.name, true
	}
	return "", false
}

func indexAnswers(index declaredIndex, path statementAccessPath) bool {
	for _, conjunct := range index.predicate {
		if !strings.Contains(path.where, conjunct) {
			return false
		}
	}
	columns := index.columns
	for len(columns) > 0 && path.equality[columns[0].name] {
		columns = columns[1:]
	}
	if len(columns) < len(path.orderBy) {
		return false
	}
	for i, want := range path.orderBy {
		if columns[i] != want {
			return false
		}
	}
	return true
}

func orderedPageStatements() []struct {
	name string
	sql  string
} {
	const table = `"looprig"."p_ordered_records"`
	return []struct {
		name string
		sql  string
	}{
		{"ordered first", orderedquery.Ordered(table, "sessions", "scope", 0, 25).SQL},
		{"ordered middle", orderedquery.Ordered(table, "sessions", "scope", 500, 25).SQL},
		{"ranked first", orderedquery.Ranked(table, "sessions", "workers", nil, 24).SQL},
		{"ranked middle", orderedquery.Ranked(table, "sessions", "workers", &orderedquery.RankedPosition{}, 24).SQL},
		{"due first", orderedquery.Due(table, "sessions", 999, nil, 24).SQL},
		{"due middle", orderedquery.Due(table, "sessions", 999, &orderedquery.DuePosition{}, 24).SQL},
	}
}

// TestOrderedIndexPageStatementsAreCoveredByADeclaredIndex is the table-scan
// prohibition, which previously had no database-free guard at all: it existed
// only as the integration plan gate, which needs -tags integration and a DSN.
// It does not replace the plan gate — only PostgreSQL can say which index it
// chose — but it holds the property that an index capable of answering each
// page without a sort is declared, and that the statement asks for it.
func TestOrderedIndexPageStatementsAreCoveredByADeclaredIndex(t *testing.T) {
	t.Parallel()
	indexes := declaredOrderedIndexes(t)
	for _, statement := range orderedPageStatements() {
		path, err := readAccessPath(statement.sql)
		if err != nil {
			t.Errorf("%s: %v: %s", statement.name, err, statement.sql)
			continue
		}
		name, ok := coveringIndex(indexes, path)
		if !ok {
			t.Errorf("%s has no declared index that answers it without a sort: %s", statement.name, statement.sql)
			continue
		}
		t.Logf("%s is answered by index %q", statement.name, name)
	}
}

func TestIndexCoverageClassifierRejectsUnbackedStatements(t *testing.T) {
	t.Parallel()
	indexes := declaredOrderedIndexes(t)
	const columns = "namespace, ordering_scope, stable_key"
	const table = `"looprig"."p_ordered_records"`
	for _, test := range []struct{ name, sql string }{
		{"no ORDER BY at all", "SELECT " + columns + " FROM " + table + " WHERE namespace = $1 AND ordering_scope = $2 LIMIT $3"},
		{"no WHERE at all", "SELECT " + columns + " FROM " + table + " ORDER BY " + table + ".order_id ASC, stable_key ASC LIMIT $3"},
		{"ordering column is not indexed", "SELECT " + columns + " FROM " + table + " WHERE namespace = $1 AND ordering_scope = $2 ORDER BY revision ASC LIMIT $3"},
		{"scope no longer pinned", "SELECT " + columns + " FROM " + table + " WHERE namespace = $1 ORDER BY " + table + ".order_id ASC, stable_key ASC LIMIT $3"},
		{"ranked direction reversed", "SELECT " + columns + " FROM " + table + " WHERE namespace = $1 AND ranking_scope = $2 AND ranked AND NOT deleted ORDER BY rank_value ASC, stable_key ASC, ordering_scope ASC LIMIT $3"},
		{"due partial-index predicate dropped", "SELECT " + columns + " FROM " + table + " WHERE namespace = $1 AND due_state = 1 ORDER BY due_at ASC, stable_key ASC, ordering_scope ASC LIMIT $3"},
		{"ordering tail truncated to a non-key column", "SELECT " + columns + " FROM " + table + " WHERE namespace = $1 AND ranking_scope = $2 AND ranked AND NOT deleted ORDER BY rank_value DESC, revision DESC LIMIT $3"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path, err := readAccessPath(test.sql)
			if err != nil {
				return
			}
			if name, ok := coveringIndex(indexes, path); ok {
				t.Fatalf("coverage classifier accepted an unbacked statement against index %q: %s", name, test.sql)
			}
		})
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
		var raw []byte
		_ = ((tx).QueryRow(ctx, ("EXPLAIN  (ANALYZE, BUFFERS, FORMAT JSON)\n"+((test.statement.SQL))), ((test.statement.Args))...)).Scan((&raw))
		var plan any
		_ = json.Unmarshal((raw), (&plan))
		if !planUsesIndex((plan), test.indexName) || planHasNodeType((plan), "Sort") || planHasNodeType(plan, "Incremental Sort") {
			panic("plan")
		}
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
	for _, test := range live {
		var raw []byte
		tx.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+test.statement.SQL, test.statement.Args...).Scan(&raw)
		var plan any
		json.Unmarshal(raw, &plan)
		_ = planUsesIndex(plan, test.indexName)
		_ = planHasNodeType(plan, "Sort")
	}`)
		if err := validatePlanGateStatementOwnership([]byte(deadTableLiveCopy)); err == nil {
			t.Fatal("ownership validation accepted a dead valid helper while the live EXPLAIN loop consumed copied SQL")
		}
	})
	t.Run("same-name local shadow supplies copied live cases", func(t *testing.T) {
		shadow := `orderedPlanCases := func(table, prefix string) []orderedPlanCase {
		_ = table
		_ = prefix
		return []orderedPlanCase{
			{statement: orderedquery.Statement{SQL: "SELECT copied order first"}},
			{statement: orderedquery.Statement{SQL: "SELECT copied order middle"}},
			{statement: orderedquery.Statement{SQL: "SELECT copied rank first"}},
			{statement: orderedquery.Statement{SQL: "SELECT copied rank middle"}},
			{statement: orderedquery.Statement{SQL: "SELECT copied due first"}},
			{statement: orderedquery.Statement{SQL: "SELECT copied due middle"}},
		}
	}
	for _, test := range orderedPlanCases(table, prefix) {
		var raw []byte
		tx.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+test.statement.SQL, test.statement.Args...).Scan(&raw)
		var plan any
		json.Unmarshal(raw, &plan)
		_ = planUsesIndex(plan, test.indexName)
		_ = planHasNodeType(plan, "Sort")
	}`
		if err := validatePlanGateStatementOwnership([]byte(planOwnershipFixtureWithLoop(deadEntries, "", shadow))); err == nil {
			t.Fatal("ownership validation accepted a same-name local shadow supplying copied live cases")
		}
	})
	t.Run("typed nil marked middle", func(t *testing.T) {
		typedNilMiddle := replaceEntry(deadEntries, 3, `{family: orderedPlanRanked, page: orderedPlanMiddle, statement: orderedquery.Ranked(table, "sessions", "rank", (*orderedquery.RankedPosition)(nil), 24)}`)
		if err := validatePlanGateStatementOwnership([]byte(planOwnershipFixture(typedNilMiddle, ""))); err == nil {
			t.Fatal("ownership validation classified a typed nil ranked cursor as a middle page")
		}
	})
}

// TestPlanGateOwnershipRejectsEveryExecutionSpellingAndUnboundAssertions covers
// the escape the previous single-name match left open: the guard collected
// calls literally named QueryRow, so a dead mandated QueryRow beside a live
// Query, Exec or SendBatch decoy satisfied it while a copied statement supplied
// the plan that was actually asserted.
func TestPlanGateOwnershipRejectsEveryExecutionSpellingAndUnboundAssertions(t *testing.T) {
	entries := []string{
		`{family: orderedPlanOrder, page: orderedPlanFirst, statement: orderedquery.Ordered(table, "sessions", "scope", 0, 25)}`,
		`{family: orderedPlanOrder, page: orderedPlanMiddle, statement: orderedquery.Ordered(table, "sessions", "scope", 500, 25)}`,
		`{family: orderedPlanRanked, page: orderedPlanFirst, statement: orderedquery.Ranked(table, "sessions", "rank", nil, 24)}`,
		`{family: orderedPlanRanked, page: orderedPlanMiddle, statement: orderedquery.Ranked(table, "sessions", "rank", &orderedquery.RankedPosition{}, 24)}`,
		`{family: orderedPlanDue, page: orderedPlanFirst, statement: orderedquery.Due(table, "sessions", 999, nil, 24)}`,
		`{family: orderedPlanDue, page: orderedPlanMiddle, statement: orderedquery.Due(table, "sessions", 999, &orderedquery.DuePosition{}, 24)}`,
	}
	const tail = `var plan any
		json.Unmarshal(raw, &plan)
		_ = planUsesIndex(plan, test.indexName)
		_ = planHasNodeType(plan, "Sort")`
	loop := func(body string) string {
		return "for _, test := range orderedPlanCases(table, prefix) {\n\t\tvar raw []byte\n\t\t" + body + "\n\t}"
	}
	tests := []struct {
		name string
		loop string
	}{
		{
			name: "dead QueryRow beside live Query decoy",
			loop: loop(`if false {
			tx.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+test.statement.SQL, test.statement.Args...)
		}
		rows, _ := tx.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) SELECT copied lookalike")
		rows.Scan(&raw)
		` + tail),
		},
		{
			name: "live Exec decoy beside the mandated QueryRow",
			loop: loop(`tx.Exec(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) SELECT copied lookalike")
		tx.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+test.statement.SQL, test.statement.Args...).Scan(&raw)
		` + tail),
		},
		{
			name: "live SendBatch decoy beside the mandated QueryRow",
			loop: loop(`tx.SendBatch(ctx, batch)
		tx.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+test.statement.SQL, test.statement.Args...).Scan(&raw)
		` + tail),
		},
		{
			name: "Scan not bound to the mandated QueryRow",
			loop: loop(`tx.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+test.statement.SQL, test.statement.Args...)
		cached.Scan(&raw)
		` + tail),
		},
		{
			name: "decoded plan is not the scanned bytes",
			loop: loop(`tx.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+test.statement.SQL, test.statement.Args...).Scan(&raw)
		var plan any
		json.Unmarshal(cachedBytes, &plan)
		_ = planUsesIndex(plan, test.indexName)
		_ = planHasNodeType(plan, "Sort")`),
		},
		{
			name: "index assertion reads another plan",
			loop: loop(`tx.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+test.statement.SQL, test.statement.Args...).Scan(&raw)
		var plan any
		json.Unmarshal(raw, &plan)
		_ = planUsesIndex(cachedPlan, test.indexName)
		_ = planHasNodeType(plan, "Sort")`),
		},
		{
			name: "downgraded EXPLAIN never executes the statement",
			loop: loop(`tx.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+test.statement.SQL, test.statement.Args...).Scan(&raw)
		` + tail),
		},
		{
			name: "row-returning decoy hoisted out of the range",
			loop: `hoisted, _ := admin.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) SELECT copied lookalike")
	_ = hoisted
	` + loop(`tx.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+test.statement.SQL, test.statement.Args...).Scan(&raw)
		`+tail),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validatePlanGateStatementOwnership([]byte(planOwnershipFixtureWithLoop(entries, "", test.loop))); err == nil {
				t.Fatal("ownership validation accepted an unbound assertion or a non-QueryRow execution spelling")
			}
		})
	}
}

// TestProductionOwnershipRejectsEveryExecutionSpelling is the production-side
// sibling of the same blindness: methodCallsNamed(body, "Query") could not see a
// live QueryRow, Exec or SendBatch standing beside the mandated Query.
func TestProductionOwnershipRejectsEveryExecutionSpelling(t *testing.T) {
	ordered := `q := orderedquery.Ordered(table, namespace, scope, after, limit); rows, _ := s.pool.Query(ctx, q.SQL, q.Args...)`
	due := `q := orderedquery.Due(table, namespace, bound, position, limit); rows, _ := s.pool.Query(ctx, q.SQL, q.Args...)`
	for _, test := range []struct{ name, ranked string }{
		{name: "live QueryRow decoy", ranked: `q := orderedquery.Ranked(table, namespace, scope, position, limit); s.pool.QueryRow(ctx, "SELECT copied").Scan(&sink); rows, _ := s.pool.Query(ctx, q.SQL, q.Args...)`},
		{name: "live Exec decoy", ranked: `q := orderedquery.Ranked(table, namespace, scope, position, limit); s.pool.Exec(ctx, "SELECT copied"); rows, _ := s.pool.Query(ctx, q.SQL, q.Args...)`},
		{name: "live SendBatch decoy", ranked: `q := orderedquery.Ranked(table, namespace, scope, position, limit); s.pool.SendBatch(ctx, batch); rows, _ := s.pool.Query(ctx, q.SQL, q.Args...)`},
		{name: "statement read through QueryRow instead of Query", ranked: `q := orderedquery.Ranked(table, namespace, scope, position, limit); s.pool.QueryRow(ctx, q.SQL, q.Args...).Scan(&sink)`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateProductionStatementOwnership([]byte(productionOwnershipFixture(ordered, test.ranked, due))); err == nil {
				t.Fatal("production ownership accepted an extra or substituted execution spelling")
			}
		})
	}
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

const mandatedPlanLoop = `for _, test := range orderedPlanCases(table, prefix) {
		var raw []byte
		tx.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+test.statement.SQL, test.statement.Args...).Scan(&raw)
		var plan any
		json.Unmarshal(raw, &plan)
		_ = planUsesIndex(plan, test.indexName)
		_ = planHasNodeType(plan, "Sort")
	}`

func planOwnershipFixture(entries []string, extra string) string {
	return planOwnershipFixtureWithLoop(entries, extra, mandatedPlanLoop)
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
	return validatePlanIntegrationLoop(file, helpers[0])
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

func validatePlanIntegrationLoop(file *ast.File, casesHelper *ast.FuncDecl) error {
	if casesHelper.Name.Obj == nil || casesHelper.Name.Obj.Kind != ast.Fun || casesHelper.Name.Obj.Decl != casesHelper {
		return fmt.Errorf("orderedPlanCases helper has no unique function declaration identity")
	}
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
		if !ok || !isHelperCall(rangeStatement.X, casesHelper) {
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
	// Nothing outside the ranged body may return rows. A hoisted decoy read is
	// otherwise free to fill the variable the plan assertions consume.
	for _, call := range executionCallsIn(test.Body) {
		if nodeContains(ranges[0].Body, call) {
			continue
		}
		if rowReturningMethods[callSelectorName(call)] {
			return fmt.Errorf("plan integration test performs a row-returning %s outside the orderedPlanCases range", callSelectorName(call))
		}
	}
	// Inside the ranged body the mandated statement is the only thing that may
	// reach the database at all. Naming QueryRow alone made the guard exactly
	// as strong as the last spelling someone thought of: a dead QueryRow beside
	// a live Query, Exec or SendBatch passed it.
	executions := executionCallsIn(ranges[0].Body)
	if len(executions) != 1 {
		return fmt.Errorf("orderedPlanCases range body has %d database execution calls (%s), want exactly the one mandated QueryRow", len(executions), strings.Join(executionCallNames(executions), ", "))
	}
	query := executions[0]
	if callSelectorName(query) != "QueryRow" {
		return fmt.Errorf("orderedPlanCases range body executes %s, want the mandated QueryRow", callSelectorName(query))
	}
	selector := unparen(query.Fun).(*ast.SelectorExpr)
	queryReceiver, ok := unparen(selector.X).(*ast.Ident)
	if !ok || !sameObject(queryReceiver, transactions[0]) {
		return fmt.Errorf("plan integration QueryRow is not called on its BeginTx result")
	}
	if len(query.Args) != 3 || query.Ellipsis == token.NoPos || !isExplainStatement(query.Args[1], rangeValue) || !isNestedSelector(query.Args[2], rangeValue, "statement", "Args") {
		return fmt.Errorf("plan integration QueryRow must explain the ranged case statement SQL with EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) and its variadic Args")
	}
	return validatePlanAssertionChain(ranges[0].Body, query)
}

// validatePlanAssertionChain binds what is asserted to what was executed. The
// mandated QueryRow being present is not the property under guard; the plan the
// index assertions read having come from it is.
func validatePlanAssertionChain(body *ast.BlockStmt, query *ast.CallExpr) error {
	scans := selectorCallsNamed(body, "Scan")
	if len(scans) != 1 {
		return fmt.Errorf("orderedPlanCases range body has %d Scan calls, want exactly 1", len(scans))
	}
	scanSelector, ok := unparen(scans[0].Fun).(*ast.SelectorExpr)
	if !ok || unparen(scanSelector.X) != ast.Expr(query) {
		return fmt.Errorf("plan integration Scan is not called directly on the mandated QueryRow result")
	}
	if len(scans[0].Args) != 1 {
		return fmt.Errorf("plan integration Scan takes %d destinations, want exactly 1", len(scans[0].Args))
	}
	raw, ok := addressedIdent(scans[0].Args[0])
	if !ok {
		return fmt.Errorf("plan integration Scan destination is not an addressed local variable")
	}
	decodes := packageCallsNamed(body, "json", "Unmarshal")
	if len(decodes) != 1 || len(decodes[0].Args) != 2 {
		return fmt.Errorf("orderedPlanCases range body has %d two-argument json.Unmarshal calls, want exactly 1", len(decodes))
	}
	source, ok := unparen(decodes[0].Args[0]).(*ast.Ident)
	if !ok || !sameObject(source, raw) {
		return fmt.Errorf("plan integration json.Unmarshal does not decode the scanned plan bytes")
	}
	plan, ok := addressedIdent(decodes[0].Args[1])
	if !ok {
		return fmt.Errorf("plan integration json.Unmarshal destination is not an addressed local variable")
	}
	for _, assertion := range []string{"planUsesIndex", "planHasNodeType"} {
		calls := freeCallsNamed(body, assertion)
		if len(calls) == 0 {
			return fmt.Errorf("orderedPlanCases range body makes no %s assertion", assertion)
		}
		for _, call := range calls {
			subject, ok := unparen(call.Args[0]).(*ast.Ident)
			if len(call.Args) == 0 || !ok || !sameObject(subject, plan) {
				return fmt.Errorf("%s does not assert over the decoded plan of the mandated QueryRow", assertion)
			}
		}
	}
	return nil
}

// executionMethods is the set of pgx entry points by which a statement can
// reach the database, and rowReturningMethods the subset that can supply a
// value an assertion later reads.
var executionMethods = map[string]bool{
	"Query": true, "QueryRow": true, "Exec": true, "SendBatch": true,
	"CopyFrom": true, "Begin": true, "BeginTx": true, "BeginFunc": true,
}

var rowReturningMethods = map[string]bool{
	"Query": true, "QueryRow": true, "SendBatch": true, "CopyFrom": true, "BeginFunc": true,
}

func executionCallsIn(root ast.Node) []*ast.CallExpr {
	var calls []*ast.CallExpr
	ast.Inspect(root, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if selector, ok := unparen(call.Fun).(*ast.SelectorExpr); ok && executionMethods[selector.Sel.Name] {
			calls = append(calls, call)
		}
		return true
	})
	return calls
}

func executionCallNames(calls []*ast.CallExpr) []string {
	names := make([]string, 0, len(calls))
	for _, call := range calls {
		names = append(names, callSelectorName(call))
	}
	return names
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
	if !ok || literal.Kind != token.STRING {
		return false
	}
	text, err := strconv.Unquote(literal.Value)
	if err != nil {
		return false
	}
	// Task step 4 names this exact form. "EXPLAIN" as a substring also matches a
	// downgrade to EXPLAIN (FORMAT JSON), which plans the statement without ever
	// running it, so the plan gate would stop measuring real execution.
	if !strings.HasPrefix(strings.Join(strings.Fields(text), " "), explainFlavour) {
		return false
	}
	return isNestedSelector(binary.Y, value, "statement", "SQL")
}

const explainFlavour = "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)"

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
		executions := executionCallsIn(function.Body)
		if len(executions) != 1 {
			return fmt.Errorf("List%s has %d database execution calls (%s), want exactly one Query", family, len(executions), strings.Join(executionCallNames(executions), ", "))
		}
		query := executions[0]
		if callSelectorName(query) != "Query" {
			return fmt.Errorf("List%s executes %s, want the single pool Query that consumes its orderedquery statement", family, callSelectorName(query))
		}
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
