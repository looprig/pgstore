package pgstore

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"reflect"
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

// reorderingFunctions are the standard-library entry points that reorder a
// slice in place but whose names do not say so: sort.Slice, sort.Stable and
// sort.Strings all reorder, and none of them contains "sort" in the identifier
// a name-only classifier reads.
var reorderingFunctions = map[string]map[string]bool{
	"sort":   {"Slice": true, "SliceStable": true, "Sort": true, "Stable": true, "Strings": true, "Ints": true, "Float64s": true},
	"slices": {"Sort": true, "SortFunc": true, "SortStableFunc": true, "SortStable": true},
}

// sortednessChecks read an order without establishing one, so they are not
// sorting entry points even though their names contain the word.
var sortednessChecks = map[string]bool{
	"IsSorted": true, "IsSortedFunc": true, "SliceIsSorted": true,
	"StringsAreSorted": true, "IntsAreSorted": true, "Float64sAreSorted": true,
}

// sortingEntryPoint classifies a call by the operation it performs. An earlier
// revision banned the sort and slices imports wholesale, which also denied
// slices.Contains, slices.Clone and sort.SearchInts — none of which reorder
// anything.
func sortingEntryPoint(qualifier, name string) bool {
	if reorderingFunctions[qualifier][name] {
		return true
	}
	if sortednessChecks[name] {
		return false
	}
	return strings.Contains(strings.ToLower(name), "sort")
}

// TestOrderedIndexNeverSortsPagesInMemory is a named-entry-point guard, and its
// reach is exactly that: it catches sort.Slice, slices.SortFunc and a helper
// called sortRecords, and it does not catch an unnamed comparison loop written
// inline. The DB-free coverage guard and the integration plan gate cannot catch
// that either — both read only SQL, and neither can observe Go-side reordering.
// Measured: an inline insertion reorder in ListRanked that inverts the page
// passes this guard, the DB-free suite, the integration plan gate and the
// internal/orderedindex integration package. What kills it is the Storage
// conformance ordering suite with a database — specifically
// TestOrderedIndexConformance/TestOrderedIndexListRankedPagesByRankAndStableKey
// — and only because the reordered page differs from the database's.
func TestOrderedIndexNeverSortsPagesInMemory(t *testing.T) {
	t.Parallel()
	for index, file := range parseOrderedProduction(t) {
		path := orderedProductionPaths[index]
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			qualifier := ""
			name := callSelectorName(call)
			if selector, ok := unparen(call.Fun).(*ast.SelectorExpr); ok {
				qualifier, _ = selectorPackage(selector)
			} else if identifier, ok := unparen(call.Fun).(*ast.Ident); ok {
				name = identifier.Name
			}
			if sortingEntryPoint(qualifier, name) {
				t.Errorf("%s calls %s; the page order is the index's order, and re-establishing it in the process makes the ordering unverifiable from the SQL", path, name)
			}
			return true
		})
	}
}

func TestSortingEntryPointClassifierReadsTheOperationNotThePackage(t *testing.T) {
	t.Parallel()
	for _, forbidden := range []struct{ qualifier, name string }{
		{"sort", "Slice"}, {"sort", "SliceStable"}, {"sort", "Sort"}, {"sort", "Stable"}, {"sort", "Strings"},
		{"slices", "Sort"}, {"slices", "SortFunc"}, {"slices", "SortStableFunc"},
		{"", "sortRecords"}, {"", "insertionSortByRank"}, {"pager", "SortPage"},
	} {
		if !sortingEntryPoint(forbidden.qualifier, forbidden.name) {
			t.Errorf("sorting classifier missed %s.%s", forbidden.qualifier, forbidden.name)
		}
	}
	for _, allowed := range []struct{ qualifier, name string }{
		{"slices", "Contains"}, {"slices", "Clone"}, {"slices", "Equal"}, {"slices", "BinarySearch"},
		{"sort", "SearchInts"}, {"sort", "Search"}, {"sort", "SliceIsSorted"},
		{"bytes", "Clone"}, {"strings", "Fields"}, {"", "scanRecords"},
	} {
		if sortingEntryPoint(allowed.qualifier, allowed.name) {
			t.Errorf("sorting classifier denied %s.%s, which reorders nothing", allowed.qualifier, allowed.name)
		}
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

// packagesOwningMigrationTables derives which internal package owns each table
// the migrations create, by finding the package whose production source names
// the table suffix. Deriving the denied set inverts a hand-written allowlist:
// widening an allowlist by one name is invisible, whereas a package that owns a
// migration table is denied by construction the moment it exists.
func packagesOwningMigrationTables(t *testing.T) map[string]string {
	t.Helper()
	suffixes := migrationTableSuffixes(t)
	owners := make(map[string]string)
	for _, path := range productionGoFiles(t) {
		directory := filepath.Dir(path)
		if !strings.HasPrefix(directory, "internal"+string(filepath.Separator)) {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(literal.Value)
			if err != nil {
				return true
			}
			for _, suffix := range suffixes {
				if value == suffix {
					owners[filepath.Base(directory)] = suffix
				}
			}
			return true
		})
	}
	if len(owners) < 4 {
		t.Fatalf("derived %d packages owning migration tables (%v); the ownership derivation no longer matches production", len(owners), owners)
	}
	return owners
}

func TestOrderedIndexNeverFallsBackToAnotherPrimitive(t *testing.T) {
	t.Parallel()
	owners := packagesOwningMigrationTables(t)
	denied := make(map[string]string)
	for pkg, suffix := range owners {
		if !strings.HasPrefix(suffix, "ordered_") {
			denied[pkg] = suffix
		}
	}
	if len(denied) < 3 {
		t.Fatalf("derived %d sibling primitive packages (%v); the fallback guard has no input", len(denied), denied)
	}

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
			if suffix, isSibling := denied[strings.TrimPrefix(importPath, prefix)]; isSibling {
				t.Errorf("%s imports %q, which owns the %q table; OrderedIndex has no fallback store", path, importPath, suffix)
			}
		}
	}

	// A package-path denial does not reach a fallback written at the table
	// level, which needs no import at all: the ordered statements may name only
	// the ordered tables.
	literals := orderedProductionStringLiterals(t)
	for _, suffix := range denied {
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
// coveringIndex reports the declared index that answers the statement without a
// sort, or the reason each declared index cannot.
func coveringIndex(indexes []declaredIndex, path statementAccessPath) (string, bool, []string) {
	var reasons []string
	for _, index := range indexes {
		if reason := indexAnswers(index, path); reason == "" {
			return index.name, true, nil
		} else {
			reasons = append(reasons, index.name+": "+reason)
		}
	}
	return "", false, reasons
}

// normalizeSQLFragment removes the cosmetic differences — surrounding
// parentheses and whitespace runs — that make two spellings of one predicate
// look unequal.
func normalizeSQLFragment(fragment string) string {
	fragment = strings.Join(strings.Fields(fragment), " ")
	for {
		trimmed := strings.TrimSpace(fragment)
		if len(trimmed) < 2 || trimmed[0] != '(' || trimmed[len(trimmed)-1] != ')' {
			break
		}
		inner, end := balancedParenthesis(trimmed, 0)
		if end != len(trimmed)-1 {
			break
		}
		fragment = inner
	}
	return strings.ReplaceAll(strings.ReplaceAll(fragment, "(", ""), ")", "")
}

// indexAnswers returns "" when the index answers the statement, and otherwise
// the reason it cannot. A partial index whose predicate the statement does not
// repeat is a fact about the migration and the statement together, so the
// reason says which side it read.
func indexAnswers(index declaredIndex, path statementAccessPath) string {
	where := normalizeSQLFragment(path.where)
	for _, conjunct := range index.predicate {
		if !strings.Contains(where, normalizeSQLFragment(conjunct)) {
			return fmt.Sprintf("it is declared in the migration as a partial index over %q, which this statement's WHERE does not repeat", conjunct)
		}
	}
	columns := index.columns
	for len(columns) > 0 && path.equality[columns[0].name] {
		columns = columns[1:]
	}
	if len(columns) < len(path.orderBy) {
		return fmt.Sprintf("only %d of its columns remain unpinned, fewer than the %d ordering columns", len(columns), len(path.orderBy))
	}
	for _, inverted := range []bool{false, true} {
		matched := true
		for i, want := range path.orderBy {
			if columns[i].name != want.name || columns[i].descending != (want.descending != inverted) {
				matched = false
				break
			}
		}
		if matched {
			return ""
		}
	}
	return "its remaining key does not match the ordering columns in either scan direction"
}

type coveredPageStatement struct {
	name   string
	family string
	page   string
	sql    string
}

// orderedPageStatements enumerates the six pages the coverage guard measures.
// It is bounded exactly as the integration plan gate's own table is bounded —
// six cases, one first and one middle per builder family — because an
// unbounded enumeration is how a seventh builder becomes silently uncovered,
// and an empty one is how the whole guard becomes vacuous.
func orderedPageStatements(t *testing.T) []coveredPageStatement {
	t.Helper()
	const table = `"looprig"."p_ordered_records"`
	statements := []coveredPageStatement{
		{"ordered first", "Ordered", "first", orderedquery.Ordered(table, "sessions", "scope", 0, 25).SQL},
		{"ordered middle", "Ordered", "middle", orderedquery.Ordered(table, "sessions", "scope", 500, 25).SQL},
		{"ranked first", "Ranked", "first", orderedquery.Ranked(table, "sessions", "workers", nil, 24).SQL},
		{"ranked middle", "Ranked", "middle", orderedquery.Ranked(table, "sessions", "workers", &orderedquery.RankedPosition{}, 24).SQL},
		{"due first", "Due", "first", orderedquery.Due(table, "sessions", 999, nil, 24).SQL},
		{"due middle", "Due", "middle", orderedquery.Due(table, "sessions", 999, &orderedquery.DuePosition{}, 24).SQL},
	}
	if len(statements) != 6 {
		t.Fatalf("coverage enumerates %d page statements, want exactly 6", len(statements))
	}
	// Ordered's cursor is a bound parameter rather than an extra predicate, so
	// its two pages share one SQL string by design; only the keyset builders
	// differ between pages.
	pages := make(map[string]map[string]int)
	byPage := make(map[string]string)
	for _, statement := range statements {
		if statement.sql == "" {
			t.Fatalf("%s produced an empty statement, so it constrains nothing", statement.name)
		}
		if statement.family != "Ordered" {
			if previous, ok := byPage[statement.family]; ok && previous == statement.sql {
				t.Fatalf("%s builds the same SQL for both pages, so the keyset predicate is not being exercised", statement.family)
			}
			byPage[statement.family] = statement.sql
		}
		if pages[statement.family] == nil {
			pages[statement.family] = make(map[string]int)
		}
		pages[statement.family][statement.page]++
	}
	for _, family := range []string{"Ordered", "Ranked", "Due"} {
		if pages[family]["first"] != 1 || pages[family]["middle"] != 1 {
			t.Fatalf("orderedquery.%s coverage = %d first/%d middle, want 1/1, matching the bound validatePlanGateStatementOwnership holds the integration table to", family, pages[family]["first"], pages[family]["middle"])
		}
	}
	return statements
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
	for _, statement := range orderedPageStatements(t) {
		path, err := readAccessPath(statement.sql)
		if err != nil {
			t.Errorf("%s: %v: %s", statement.name, err, statement.sql)
			continue
		}
		name, ok, reasons := coveringIndex(indexes, path)
		if !ok {
			t.Errorf("%s is not answered without a sort by any index the migration declares.\n  statement: %s\n  %s", statement.name, statement.sql, strings.Join(reasons, "\n  "))
			continue
		}
		t.Logf("%s is answered by index %q", statement.name, name)
	}
}

// TestIndexCoverageClassifierAcceptsLegalAccessPaths pins the shapes the
// classifier must not call a table scan. Reverse pagination is the obvious next
// feature for a keyset pager, and PostgreSQL answers an exactly-reversed order
// with a backward index scan: measured on the real rank index shape, the plan
// is "Index Only Scan Backward using ..._rank_idx" with no Sort node. An
// earlier revision compared the direction of every column literally, so it
// would have reported that legal plan as unbacked while the integration plan
// gate correctly passed it. Cosmetic parentheses around a partial index's
// predicate in the migration must not change the answer either.
func TestIndexCoverageClassifierAcceptsLegalAccessPaths(t *testing.T) {
	t.Parallel()
	indexes := declaredOrderedIndexes(t)
	const columns = "namespace, ordering_scope, stable_key"
	const table = `"looprig"."p_ordered_records"`
	for _, test := range []struct{ name, sql string }{
		{"ranked page read backwards", "SELECT " + columns + " FROM " + table + " WHERE namespace = $1 AND ranking_scope = $2 AND ranked AND NOT deleted ORDER BY rank_value ASC, stable_key ASC, ordering_scope ASC LIMIT $3"},
		{"due page read backwards", "SELECT " + columns + " FROM " + table + " WHERE namespace = $1 AND due_state = 1 AND NOT deleted AND due_at <= $2 ORDER BY due_at DESC, stable_key DESC, ordering_scope DESC LIMIT $3"},
		{"ordered page read backwards", "SELECT " + columns + " FROM " + table + " WHERE namespace = $1 AND ordering_scope = $2 ORDER BY order_id DESC, stable_key DESC LIMIT $3"},
		{"partial predicate written with parentheses", "SELECT " + columns + " FROM " + table + " WHERE namespace = $1 AND (due_state = 1) AND (NOT deleted) AND due_at <= $2 ORDER BY due_at ASC, stable_key ASC, ordering_scope ASC LIMIT $3"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path, err := readAccessPath(test.sql)
			if err != nil {
				t.Fatalf("readAccessPath: %v", err)
			}
			if _, ok, reasons := coveringIndex(indexes, path); !ok {
				t.Fatalf("coverage classifier rejected a legal access path:\n  %s", strings.Join(reasons, "\n  "))
			}
		})
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
		{"due partial-index predicate dropped", "SELECT " + columns + " FROM " + table + " WHERE namespace = $1 AND due_state = 1 ORDER BY due_at ASC, stable_key ASC, ordering_scope ASC LIMIT $3"},
		{"ordering tail truncated to a non-key column", "SELECT " + columns + " FROM " + table + " WHERE namespace = $1 AND ranking_scope = $2 AND ranked AND NOT deleted ORDER BY rank_value DESC, revision DESC LIMIT $3"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path, err := readAccessPath(test.sql)
			if err != nil {
				return
			}
			if name, ok, _ := coveringIndex(indexes, path); ok {
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

// validPlanEntries is the single legal plan-case table the fixtures start from.
func validPlanEntries() []string {
	return []string{
		`{family: orderedPlanOrder, page: orderedPlanFirst, statement: orderedquery.Ordered(table, "sessions", "scope", 0, 25)}`,
		`{family: orderedPlanOrder, page: orderedPlanMiddle, statement: orderedquery.Ordered(table, "sessions", "scope", 500, 25)}`,
		`{family: orderedPlanRanked, page: orderedPlanFirst, statement: orderedquery.Ranked(table, "sessions", "rank", nil, 24)}`,
		`{family: orderedPlanRanked, page: orderedPlanMiddle, statement: orderedquery.Ranked(table, "sessions", "rank", &orderedquery.RankedPosition{}, 24)}`,
		`{family: orderedPlanDue, page: orderedPlanFirst, statement: orderedquery.Due(table, "sessions", 999, nil, 24)}`,
		`{family: orderedPlanDue, page: orderedPlanMiddle, statement: orderedquery.Due(table, "sessions", 999, &orderedquery.DuePosition{}, 24)}`,
	}
}

func TestPlanGateStatementOwnershipRejectsCopiesAndUnusedBuilders(t *testing.T) {
	validEntries := validPlanEntries()
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
	deadEntries := validPlanEntries()
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
	entries := validPlanEntries()
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
			name: "live Query decoy on the plan transaction",
			loop: loop(`tx.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) SELECT copied lookalike")
		tx.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+test.statement.SQL, test.statement.Args...).Scan(&raw)
		` + tail),
		},
		{
			name: "live QueryFunc decoy on the plan transaction",
			loop: loop(`tx.QueryFunc(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) SELECT copied lookalike", nil, nil, nil)
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
			name: "plan read hoisted out of the range onto the same transaction",
			loop: `hoisted, _ := tx.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) SELECT copied lookalike")
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

// TestPlanGateOwnershipAllowsSeedingAndPerCasePlannerControls pins the shapes
// receiver binding deliberately permits. An earlier revision counted call names
// over the whole body, which rejected all three: CopyFrom is the idiomatic pgx
// bulk load and this gate seeds ten thousand rows, per-case planner controls
// and ANALYZE are Exec, and neither can supply the plan that is asserted. A
// guard that forbids them buys nothing and costs a legal implementation.
func TestPlanGateOwnershipAllowsSeedingAndPerCasePlannerControls(t *testing.T) {
	t.Parallel()
	entries := validPlanEntries()
	const tail = `var plan any
		json.Unmarshal(raw, &plan)
		_ = planUsesIndex(plan, test.indexName)
		_ = planHasNodeType(plan, "Sort")`
	for _, test := range []struct{ name, loop string }{
		{
			name: "seeded with CopyFrom and analysed before the range",
			loop: `admin.CopyFrom(ctx, pgx.Identifier{"records"}, columns, source)
	admin.Exec(ctx, "ANALYZE "+table)
	rows, _ := admin.Query(ctx, "SELECT count(*) FROM "+table)
	_ = rows
	for _, test := range orderedPlanCases(table, prefix) {
		var raw []byte
		tx.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+test.statement.SQL, test.statement.Args...).Scan(&raw)
		` + tail + `
	}`,
		},
		{
			name: "per-case planner controls and ANALYZE inside the range",
			loop: `for _, test := range orderedPlanCases(table, prefix) {
		tx.Exec(ctx, "SET LOCAL enable_seqscan = off")
		tx.Exec(ctx, "ANALYZE "+table)
		var raw []byte
		tx.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+test.statement.SQL, test.statement.Args...).Scan(&raw)
		` + tail + `
	}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validatePlanGateStatementOwnership([]byte(planOwnershipFixtureWithLoop(entries, "", test.loop))); err != nil {
				t.Fatalf("ownership validation rejected a legal seeding or planner-control shape: %v", err)
			}
		})
	}
}

// TestRowReturningMethodSetIsFullyExercised pins the size of the shared set so
// a member cannot be deleted silently. Each member is exercised as a decoy by
// the plan-gate and production spelling tests above and below; deleting one
// would let that spelling through, and the count is what notices.
func TestRowReturningMethodSetIsFullyExercised(t *testing.T) {
	t.Parallel()
	want := map[string]bool{"Query": true, "QueryRow": true, "QueryFunc": true, "SendBatch": true}
	if len(rowReturningMethods) != len(want) {
		t.Fatalf("rowReturningMethods = %v, want exactly %v; every member must be pinned by a decoy fixture", rowReturningMethods, want)
	}
	for name := range want {
		if !rowReturningMethods[name] {
			t.Errorf("rowReturningMethods lost %q", name)
		}
	}
}

// TestProductionOwnershipRejectsEveryExecutionSpelling is the production-side
// sibling of the same blindness: methodCallsNamed(body, "Query") could not see a
// live QueryRow, Exec or SendBatch standing beside the mandated Query.
func TestProductionOwnershipRejectsEveryExecutionSpelling(t *testing.T) {
	ordered := `q := orderedquery.Ordered(table, namespace, scope, after, limit); rows, _ := s.pool.Query(ctx, q.SQL, q.Args...); scanRecords(rows)`
	due := `q := orderedquery.Due(table, namespace, bound, position, limit); rows, _ := s.pool.Query(ctx, q.SQL, q.Args...); scanRecords(rows)`
	for _, test := range []struct{ name, ranked string }{
		{name: "live QueryRow decoy", ranked: `q := orderedquery.Ranked(table, namespace, scope, position, limit); s.pool.QueryRow(ctx, "SELECT copied").Scan(&sink); rows, _ := s.pool.Query(ctx, q.SQL, q.Args...); scanRecords(rows)`},
		{name: "live QueryFunc decoy", ranked: `q := orderedquery.Ranked(table, namespace, scope, position, limit); s.pool.QueryFunc(ctx, "SELECT copied", nil, nil, nil); rows, _ := s.pool.Query(ctx, q.SQL, q.Args...); scanRecords(rows)`},
		{name: "live read inside a transaction opened from the same pool", ranked: `q := orderedquery.Ranked(table, namespace, scope, position, limit); tx, _ := s.pool.BeginTx(ctx, opts); tx.Query(ctx, "SELECT copied"); rows, _ := s.pool.Query(ctx, q.SQL, q.Args...); scanRecords(rows)`},
		{name: "live read on another store's pool supplies the scanned rows", ranked: `q := orderedquery.Ranked(table, namespace, scope, position, limit); dead, _ := s.pool.Query(ctx, q.SQL, q.Args...); _ = dead; rows, _ := other.pool.Query(ctx, "SELECT copied"); scanRecords(rows)`},
		{name: "live SendBatch decoy", ranked: `q := orderedquery.Ranked(table, namespace, scope, position, limit); s.pool.SendBatch(ctx, batch); rows, _ := s.pool.Query(ctx, q.SQL, q.Args...); scanRecords(rows)`},
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
		`q := orderedquery.Ordered(table, namespace, scope, after, limit); rows, _ := ((s.pool.Query))(ctx, (q.SQL), (q.Args)...); scanRecords(rows)`,
		`rankStatement := orderedquery.Ranked(table, namespace, scope, position, limit); rows, _ := s.pool.Query(ctx, rankStatement.SQL, rankStatement.Args...); scanRecords(rows)`,
		`dueStatement := (orderedquery.Due(table, namespace, bound, position, limit)); rows, _ := s.pool.Query(ctx, dueStatement.SQL, dueStatement.Args...); scanRecords(rows)`,
	)
	if err := validateProductionStatementOwnership([]byte(valid)); err != nil {
		t.Fatalf("production ownership rejected direct consumed builders: %v", err)
	}

	tests := []struct {
		name   string
		ranked string
	}{
		{name: "unused builder cannot satisfy ownership", ranked: `_ = orderedquery.Ranked(table, namespace, scope, position, limit); q := orderedquery.Statement{SQL: "SELECT copied"}; rows, _ := s.pool.Query(ctx, q.SQL, q.Args...); scanRecords(rows)`},
		{name: "shadowed unused builder cannot satisfy ownership", ranked: `q := orderedquery.Statement{SQL: "SELECT copied"}; { q := orderedquery.Ranked(table, namespace, scope, position, limit); _ = q }; rows, _ := s.pool.Query(ctx, q.SQL, q.Args...); scanRecords(rows)`},
		{name: "builder result not consumed", ranked: `q := orderedquery.Ranked(table, namespace, scope, position, limit); copied := orderedquery.Statement{SQL: "SELECT copied"}; rows, _ := s.pool.Query(ctx, copied.SQL, copied.Args...); scanRecords(rows)`},
		{name: "wrong family", ranked: `q := orderedquery.Due(table, namespace, bound, position, limit); rows, _ := s.pool.Query(ctx, q.SQL, q.Args...); scanRecords(rows)`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := productionOwnershipFixture(
				`q := orderedquery.Ordered(table, namespace, scope, after, limit); rows, _ := s.pool.Query(ctx, q.SQL, q.Args...); scanRecords(rows)`,
				test.ranked,
				`q := orderedquery.Due(table, namespace, bound, position, limit); rows, _ := s.pool.Query(ctx, q.SQL, q.Args...); scanRecords(rows)`,
			)
			if err := validateProductionStatementOwnership([]byte(source)); err == nil {
				t.Fatal("production ownership accepted an unused, unconsumed, or wrong-family builder")
			}
		})
	}
}

func TestProductionOwnershipRejectsDecoyQueriesAndReceiverConfusion(t *testing.T) {
	ordered := `q := orderedquery.Ordered(table, namespace, scope, after, limit); rows, _ := s.pool.Query(ctx, q.SQL, q.Args...); scanRecords(rows)`
	due := `q := orderedquery.Due(table, namespace, bound, position, limit); rows, _ := s.pool.Query(ctx, q.SQL, q.Args...); scanRecords(rows)`
	tests := []struct {
		name   string
		ranked string
	}{
		{name: "dead second receiver query", ranked: `q := orderedquery.Ranked(table, namespace, scope, position, limit); if false { s.pool.Query(ctx, "SELECT decoy") }; rows, _ := s.pool.Query(ctx, q.SQL, q.Args...); scanRecords(rows)`},
		{name: "copied live query with dead valid query", ranked: `valid := orderedquery.Ranked(table, namespace, scope, position, limit); if false { s.pool.Query(ctx, valid.SQL, valid.Args...) }; copied := orderedquery.Statement{SQL: "SELECT copied"}; rows, _ := s.pool.Query(ctx, copied.SQL, copied.Args...); scanRecords(rows)`},
		{name: "receiver shadow", ranked: `q := orderedquery.Ranked(table, namespace, scope, position, limit); { s := other; rows, _ := s.pool.Query(ctx, q.SQL, q.Args...); scanRecords(rows) }`},
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
	duplicate := productionOwnershipFixture(ordered, `copied := orderedquery.Statement{SQL: "SELECT copied"}; rows, _ := s.pool.Query(ctx, copied.SQL, copied.Args...); scanRecords(rows)`, due) +
		`func (s *Store) ListRanked() { q := orderedquery.Ranked(table, namespace, scope, position, limit); rows, _ := s.pool.Query(ctx, q.SQL, q.Args...); scanRecords(rows) }`
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
	// Only the transaction under test may read rows. Binding to the receiver,
	// rather than counting call names over the whole body, is what lets the
	// gate seed with admin.CopyFrom, set per-case planner controls with
	// tx.Exec, or ANALYZE inside the loop, while still refusing a second
	// source for the plan that is asserted.
	transaction := transactions[0]
	for _, call := range rowReturningCallsOn(test.Body, []*ast.Ident{transaction}) {
		if !nodeContains(ranges[0].Body, call) {
			return fmt.Errorf("plan integration test reads rows from the plan transaction with %s outside the orderedPlanCases range; the asserted plan must have exactly one source", callSelectorName(call))
		}
	}
	reads := rowReturningCallsOn(ranges[0].Body, []*ast.Ident{transaction})
	if len(reads) != 1 {
		return fmt.Errorf("orderedPlanCases range body reads rows from the plan transaction %d times (%s). The asserted plan must come from the one mandated statement, so exactly one QueryRow is allowed.", len(reads), strings.Join(callNames(reads), ", "))
	}
	query := reads[0]
	if callSelectorName(query) != "QueryRow" {
		return fmt.Errorf("orderedPlanCases range body reads the plan with %s; the mandated form is a single-row QueryRow whose Scan result the index assertions read", callSelectorName(query))
	}
	selector := unparen(query.Fun).(*ast.SelectorExpr)
	queryReceiver, ok := unparen(selector.X).(*ast.Ident)
	if !ok || !sameObject(queryReceiver, transaction) {
		return fmt.Errorf("plan integration QueryRow is not called on its BeginTx result")
	}
	if len(query.Args) != 3 || query.Ellipsis == token.NoPos || !isExplainStatement(query.Args[1], rangeValue) || !isNestedSelector(query.Args[2], rangeValue, "statement", "Args") {
		return fmt.Errorf("plan integration QueryRow must explain the ranged case statement SQL with %s and its variadic Args", explainFlavour)
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
		// Bound to the receiver's pool and to any transaction opened from it,
		// and restricted to calls that can actually produce the page. A
		// decoy Exec, CopyFrom or BeginTx cannot return rows, so counting it
		// would reject a legal implementation without protecting anything.
		receivers := append([]*ast.Ident{receiver}, bindingsFromCall(function.Body, "BeginTx", []*ast.Ident{receiver})...)
		reads := rowReturningCallsOn(function.Body, receivers)
		if len(reads) != 1 {
			return fmt.Errorf("List%s reads rows %d times (%s). The page must come from the one orderedquery statement, so exactly one Query is allowed.", family, len(reads), strings.Join(callNames(reads), ", "))
		}
		query := reads[0]
		if callSelectorName(query) != "Query" {
			return fmt.Errorf("List%s reads its page with %s; a page is many rows and must be read with Query so every row is scanned", family, callSelectorName(query))
		}
		if !isReceiverPoolCall(query, receiver) || len(query.Args) != 3 || query.Ellipsis == token.NoPos ||
			!isStatementSelector(query.Args[1], statements[0], "SQL") ||
			!isStatementSelector(query.Args[2], statements[0], "Args") {
			return fmt.Errorf("List%s Query is not its receiver-bound pool consuming the one orderedquery.%s statement SQL and variadic Args", family, family)
		}
		// Binding to a receiver is not enough on its own: a live read on some
		// other store's pool could still supply the page while the mandated
		// call sat unread. The rows that are scanned must be these rows.
		if err := bindsScannedRows(function.Body, query); err != nil {
			return fmt.Errorf("List%s: %w", family, err)
		}
	}
	return nil
}

// bindsScannedRows requires the rows the method decodes to be the rows the
// mandated Query returned.
func bindsScannedRows(body *ast.BlockStmt, query *ast.CallExpr) error {
	var rows *ast.Ident
	ast.Inspect(body, func(node ast.Node) bool {
		assignment, ok := node.(*ast.AssignStmt)
		if !ok || len(assignment.Lhs) == 0 || len(assignment.Rhs) == 0 || unparen(assignment.Rhs[0]) != ast.Expr(query) {
			return true
		}
		if identifier, ok := assignment.Lhs[0].(*ast.Ident); ok {
			rows = identifier
		}
		return true
	})
	if rows == nil {
		return fmt.Errorf("the mandated Query result is not bound to a variable, so nothing proves its rows are the ones decoded")
	}
	scans := freeCallsNamed(body, "scanRecords")
	if len(scans) != 1 || len(scans[0].Args) != 1 {
		return fmt.Errorf("has %d single-argument scanRecords calls, want exactly 1", len(scans))
	}
	scanned, ok := unparen(scans[0].Args[0]).(*ast.Ident)
	if !ok || !sameObject(scanned, rows) {
		return fmt.Errorf("scanRecords decodes rows that did not come from the mandated Query")
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

// TestOrderedIndexMigrationCarriesExactPhysicalIndexes asserts over the indexes
// declaredOrderedIndexes derives from the migration, rather than over a second
// hand-written copy of the same definitions. The hand copy is the one that
// drifts: it agrees with the migration only until someone edits one of them.
func TestOrderedIndexMigrationCarriesExactPhysicalIndexes(t *testing.T) {
	t.Parallel()
	indexes := declaredOrderedIndexes(t)
	got := make(map[string][]indexColumn)
	for _, index := range indexes {
		got[index.name] = index.columns
	}
	want := map[string][]indexColumn{
		"primary key":       {{name: "namespace"}, {name: "ordering_scope"}, {name: "stable_key"}},
		"ordered_order_idx": {{name: "namespace"}, {name: "ordering_scope"}, {name: "order_id"}, {name: "stable_key"}},
		"ordered_rank_idx":  {{name: "namespace"}, {name: "ranking_scope"}, {name: "rank_value", descending: true}, {name: "stable_key", descending: true}, {name: "ordering_scope", descending: true}},
		"ordered_due_idx":   {{name: "namespace"}, {name: "due_state"}, {name: "due_at"}, {name: "stable_key"}, {name: "ordering_scope"}},
	}
	for name, columns := range want {
		if !reflect.DeepEqual(got[name], columns) {
			t.Errorf("migration declares index %q as %v, want %v", name, got[name], columns)
		}
	}
	for name := range got {
		if _, expected := want[name]; !expected && name != "primary key" {
			t.Errorf("migration declares an index %q this guard does not describe; a new access path must be justified here", name)
		}
	}

	source, err := os.ReadFile("migrations/0003_ordered_index.sql")
	if err != nil {
		t.Fatal(err)
	}
	statement := string(source)
	if !strings.Contains(statement, "stable_key bytea NOT NULL") || strings.Contains(statement, "stable_key text") {
		t.Fatal("OrderedIndex migration cannot represent embedded-NUL StableKeys unless stable_key is bytea")
	}
	if strings.Contains(statement, "due_state, ordering_scope") {
		t.Fatal("due index invented a scope filter absent from Storage v0.6.0 ListDue")
	}
}

// resolveStringExpression renders the literal parts of a string expression,
// following package-level string constants and concatenation and standing in
// U+FFFD for any operand it cannot read. The literal parts are what reaches
// PostgreSQL; a comment carrying the same words is not.
func resolveStringExpression(expression ast.Expr, constants map[string]string) string {
	switch operand := unparen(expression).(type) {
	case *ast.BasicLit:
		if operand.Kind != token.STRING {
			return "\uFFFD"
		}
		value, err := strconv.Unquote(operand.Value)
		if err != nil {
			return "\uFFFD"
		}
		return value
	case *ast.Ident:
		if value, ok := constants[operand.Name]; ok {
			return value
		}
		return "\uFFFD"
	case *ast.BinaryExpr:
		if operand.Op != token.ADD {
			return "\uFFFD"
		}
		return resolveStringExpression(operand.X, constants) + resolveStringExpression(operand.Y, constants)
	}
	return "\uFFFD"
}

func packageStringConstants(file *ast.File) map[string]string {
	constants := make(map[string]string)
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
			literal, ok := unparen(value.Values[0]).(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				continue
			}
			if unquoted, err := strconv.Unquote(literal.Value); err == nil {
				constants[value.Names[0].Name] = unquoted
			}
		}
	}
	return constants
}

func enclosingFunctionName(file *ast.File, node ast.Node) string {
	name := "(file scope)"
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Body != nil && nodeContains(function.Body, node) {
			name = function.Name.Name
		}
	}
	return name
}

// TestOrderedIndexMutationsPinReadCommittedAndExplicitRowLocks reads the parsed
// tree, not the raw file. The earlier revision used strings.Count over the
// source, which this file's own header records as an evadable shape: three real
// transactions downgraded to RepeatableRead with three copies of the expected
// string parked in a package comment passed it, and a behaviour-preserving
// rename of getForUpdate failed it. Both directions are fixed by binding the
// assertion to the BeginTx options and to the call that carries the lock
// suffix, rather than to the words used to spell them.
func TestOrderedIndexMutationsPinReadCommittedAndExplicitRowLocks(t *testing.T) {
	t.Parallel()
	const path = "internal/orderedindex/orderedindex.go"
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	constants := packageStringConstants(file)

	transactions := 0
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || callSelectorName(call) != "BeginTx" || len(call.Args) != 2 {
			return true
		}
		transactions++
		where := enclosingFunctionName(file, call)
		options, ok := unparen(call.Args[1]).(*ast.CompositeLit)
		if !ok {
			t.Errorf("%s: BeginTx options are not a literal, so the isolation level cannot be read here", where)
			return true
		}
		level, err := keyedField(options, "IsoLevel")
		if err != nil {
			t.Errorf("%s: BeginTx %v; an ordered mutation must pin its isolation level explicitly rather than inherit the session default", where, err)
			return true
		}
		selector, ok := unparen(level).(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "ReadCommitted" {
			t.Errorf("%s: BeginTx runs at %s; the ordered mutation protocol serializes on the per-scope counter row and the authoritative record row, so it must run at pgx.ReadCommitted", where, types.ExprString(level))
		}
		return true
	})
	if transactions != 3 {
		t.Fatalf("orderedindex opens %d transactions with explicit options, want the three mutation paths (create, update, delete)", transactions)
	}

	// The per-scope counter lock, read from the statement that is executed.
	counterLocks := 0
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || callSelectorName(call) != "QueryRow" || len(call.Args) < 2 {
			return true
		}
		statement := resolveStringExpression(call.Args[1], constants)
		if strings.Contains(statement, "WHERE namespace = $1 AND ordering_scope = $2 FOR UPDATE") {
			counterLocks++
		}
		return true
	})
	if counterLocks != 1 {
		t.Errorf("orderedindex executes %d statements that lock the per-scope counter row with FOR UPDATE, want exactly 1: concurrent creates in one ordering scope serialize on that row", counterLocks)
	}

	// The shared authoritative-row lock, bound to the call that carries it
	// rather than to the name of the helper that makes the call.
	var lockingReaders []string
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || callSelectorName(call) != "getFrom" || len(call.Args) != 4 {
				return true
			}
			if strings.Contains(resolveStringExpression(call.Args[3], constants), "FOR UPDATE") {
				lockingReaders = append(lockingReaders, function.Name.Name)
			}
			return true
		})
	}
	if len(lockingReaders) != 1 {
		t.Fatalf("orderedindex has %d row readers that append FOR UPDATE (%s), want exactly 1 shared by update and delete", len(lockingReaders), strings.Join(lockingReaders, ", "))
	}
	for _, mutation := range []string{"updateOnce", "deleteOnce"} {
		functions := freeFunctionsNamed(file, mutation)
		methods := storeMethodsNamed(file, mutation)
		var body *ast.BlockStmt
		switch {
		case len(methods) == 1:
			body = methods[0].function.Body
		case len(functions) == 1:
			body = functions[0].Body
		default:
			t.Errorf("orderedindex has no single %s to check for the authoritative row lock", mutation)
			continue
		}
		if len(selectorCallsNamed(body, lockingReaders[0])) != 1 {
			t.Errorf("%s does not read its record through %s, the one reader that takes the authoritative row lock; without it a concurrent revision check is not serialized", mutation, lockingReaders[0])
		}
	}
}
