//go:build integration

package pgstore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	pginternal "github.com/looprig/pgstore/internal/postgres"
)

func TestOrderedIndexPlansUseExactKeysetIndexesForFirstAndMiddlePages(t *testing.T) {
	if os.Getenv("PGSTORE_TEST_DSN") == "" {
		t.Skip("PGSTORE_TEST_DSN is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	prefix := fmt.Sprintf("plan%x_%x_", time.Now().UnixNano(), conformanceStoreID.Add(1))
	store, err := Open(ctx, Options{DSN: os.Getenv("PGSTORE_TEST_DSN"), TablePrefix: prefix, Migrations: MigrationApply, AllowInsecureLocalhostOnly: true})
	if err != nil {
		t.Fatalf("Open plan store: %v", err)
	}
	defer store.Close()
	admin := adminPool(t)
	table := pginternal.Qualified("looprig", prefix+"ordered_records")
	_, err = admin.Exec(ctx, `INSERT INTO `+table+` (namespace, ordering_scope, stable_key, ranking_scope, revision, order_id, value, value_is_nil, ranked, rank_value, due_state, due_at, deleted)
SELECT 'sessions', 'scope-' || (g % 10)::text, 'key-' || lpad(((g - 1) / 10)::text, 6, '0'), 'workers', 1, ((g - 1) / 10) + 1,
       convert_to('value', 'UTF8'), false, true, (g % 100), 1, (g % 1000), false
FROM generate_series(1, 10000) AS g`)
	if err != nil {
		t.Fatalf("seed representative ordered cardinality: %v", err)
	}
	if _, err := admin.Exec(ctx, "ANALYZE "+table); err != nil {
		t.Fatalf("ANALYZE ordered records: %v", err)
	}

	tx, err := admin.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin plan transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	// These planner controls remove cost-luck from the assertion while retaining
	// the optimizer's choice among all usable indexes. An unusable or wrongly
	// ordered index still cannot satisfy the asserted Index Name.
	if _, err := tx.Exec(ctx, "SET LOCAL enable_seqscan = off; SET LOCAL enable_bitmapscan = off"); err != nil {
		t.Fatalf("set deterministic planner controls: %v", err)
	}

	tests := []struct {
		name      string
		indexName string
		query     string
		args      []any
	}{
		{name: "order first", indexName: prefix + "ordered_order_idx", query: `SELECT ` + orderedExplainColumns + ` FROM ` + table + ` WHERE namespace=$1 AND ordering_scope=$2 AND order_id > $3::numeric ORDER BY order_id ASC, stable_key ASC LIMIT 25`, args: []any{"sessions", "scope-1", "0"}},
		{name: "order middle", indexName: prefix + "ordered_order_idx", query: `SELECT ` + orderedExplainColumns + ` FROM ` + table + ` WHERE namespace=$1 AND ordering_scope=$2 AND order_id > $3::numeric ORDER BY order_id ASC, stable_key ASC LIMIT 25`, args: []any{"sessions", "scope-1", "500"}},
		{name: "rank first", indexName: prefix + "ordered_rank_idx", query: `SELECT ` + orderedExplainColumns + ` FROM ` + table + ` WHERE namespace=$1 AND ranking_scope=$2 AND ranked AND NOT deleted ORDER BY rank_value DESC, stable_key DESC, ordering_scope DESC LIMIT 25`, args: []any{"sessions", "workers"}},
		{name: "rank middle", indexName: prefix + "ordered_rank_idx", query: `SELECT ` + orderedExplainColumns + ` FROM ` + table + ` WHERE namespace=$1 AND ranking_scope=$2 AND ranked AND NOT deleted AND (rank_value,stable_key,ordering_scope) < ($3,$4,$5) ORDER BY rank_value DESC, stable_key DESC, ordering_scope DESC LIMIT 25`, args: []any{"sessions", "workers", int64(50), "key-000500", "scope-5"}},
		// Storage v0.6.0 ListDue has no scope argument. Both pages must use the
		// namespace+state+due tuple index, proving the runbook shorthand was not
		// accidentally implemented as an API-incompatible scope filter.
		{name: "due first no invented scope", indexName: prefix + "ordered_due_idx", query: `SELECT ` + orderedExplainColumns + ` FROM ` + table + ` WHERE namespace=$1 AND due_state=1 AND NOT deleted AND due_at <= $2 ORDER BY due_at ASC, stable_key ASC, ordering_scope ASC LIMIT 25`, args: []any{"sessions", int64(999)}},
		{name: "due middle no invented scope", indexName: prefix + "ordered_due_idx", query: `SELECT ` + orderedExplainColumns + ` FROM ` + table + ` WHERE namespace=$1 AND due_state=1 AND NOT deleted AND due_at <= $2 AND (due_at,stable_key,ordering_scope) > ($3,$4,$5) ORDER BY due_at ASC, stable_key ASC, ordering_scope ASC LIMIT 25`, args: []any{"sessions", int64(999), int64(500), "key-000500", "scope-5"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var raw []byte
			if err := tx.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+test.query, test.args...).Scan(&raw); err != nil {
				t.Fatalf("EXPLAIN: %v", err)
			}
			var plan any
			if err := json.Unmarshal(raw, &plan); err != nil {
				t.Fatalf("decode plan: %v", err)
			}
			if !planUsesIndex(plan, test.indexName) {
				t.Fatalf("plan did not select intended index %q: %s", test.indexName, raw)
			}
			if planHasNodeType(plan, "Sort") || planHasNodeType(plan, "Incremental Sort") {
				t.Fatalf("intended index %q required an explicit sort: %s", test.indexName, raw)
			}
		})
	}
}

func planHasNodeType(node any, expected string) bool {
	switch value := node.(type) {
	case map[string]any:
		if name, ok := value["Node Type"].(string); ok && name == expected {
			return true
		}
		for _, child := range value {
			if planHasNodeType(child, expected) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if planHasNodeType(child, expected) {
				return true
			}
		}
	}
	return false
}

const orderedExplainColumns = "namespace, ordering_scope, stable_key, ranking_scope, revision, order_id, value, value_is_nil, ranked, rank_value, due_state, due_at, deleted"

func planUsesIndex(node any, expected string) bool {
	switch value := node.(type) {
	case map[string]any:
		if name, ok := value["Index Name"].(string); ok && name == expected {
			return true
		}
		for _, child := range value {
			if planUsesIndex(child, expected) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if planUsesIndex(child, expected) {
				return true
			}
		}
	}
	return false
}
