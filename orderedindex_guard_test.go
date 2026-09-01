package pgstore

import (
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
	for _, copiedSQL := range []string{
		"order_id > $3::numeric",
		"ranking_scope=$2 AND ranked",
		"due_state=1 AND NOT deleted",
	} {
		if strings.Contains(string(planGate), copiedSQL) {
			t.Errorf("ordered plan gate duplicates production SQL fragment %q", copiedSQL)
		}
	}
	for _, builder := range []string{"orderedquery.Ordered(", "orderedquery.Ranked(", "orderedquery.Due("} {
		if !strings.Contains(string(production), builder) {
			t.Errorf("OrderedIndex production does not use shared statement builder %q", builder)
		}
		if !strings.Contains(string(planGate), builder) {
			t.Errorf("ordered plan gate does not use shared statement builder %q", builder)
		}
	}
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
