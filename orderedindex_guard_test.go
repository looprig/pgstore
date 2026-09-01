package pgstore

import (
	"os"
	"strings"
	"testing"
)

func TestOrderedIndexQueriesRemainIndexBackedKeysets(t *testing.T) {
	source, err := os.ReadFile("internal/orderedindex/orderedindex.go")
	if err != nil {
		t.Fatal(err)
	}
	lower := strings.ToLower(string(source))
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
		if !strings.Contains(string(source), required) {
			t.Errorf("OrderedIndex source lost required keyset fragment %q", required)
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
		"PRIMARY KEY (namespace, ordering_scope, stable_key)",
		"(namespace, ordering_scope, order_id, stable_key)",
		"(namespace, ranking_scope, rank_value DESC, stable_key DESC, ordering_scope DESC)",
		"(namespace, due_state, due_at, stable_key, ordering_scope)",
	} {
		if !strings.Contains(statement, required) {
			t.Errorf("OrderedIndex migration lost exact index %q", required)
		}
	}
	if strings.Contains(statement, "due_state, ordering_scope") {
		t.Fatal("due index invented a scope filter absent from Storage v0.6.0 ListDue")
	}
}
