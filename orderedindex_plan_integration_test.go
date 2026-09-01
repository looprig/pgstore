//go:build integration

package pgstore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/looprig/pgstore/internal/orderedquery"
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
SELECT 'sessions', 'scope-' || (g % 10)::text, convert_to('key-' || lpad(((g - 1) / 10)::text, 6, '0'), 'UTF8'), 'workers', 1, ((g - 1) / 10) + 1,
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
		statement orderedquery.Statement
		wantArgs  []any
	}{
		{name: "order first", indexName: prefix + "ordered_order_idx", statement: orderedquery.Ordered(table, "sessions", "scope-1", 0, 25), wantArgs: []any{"sessions", "scope-1", "0", 25}},
		{name: "order middle", indexName: prefix + "ordered_order_idx", statement: orderedquery.Ordered(table, "sessions", "scope-1", 500, 25), wantArgs: []any{"sessions", "scope-1", "500", 25}},
		{name: "rank first", indexName: prefix + "ordered_rank_idx", statement: orderedquery.Ranked(table, "sessions", "workers", nil, 24), wantArgs: []any{"sessions", "workers", 25}},
		{name: "rank middle", indexName: prefix + "ordered_rank_idx", statement: orderedquery.Ranked(table, "sessions", "workers", &orderedquery.RankedPosition{Rank: 50, StableKey: []byte("key-000500"), OrderingScope: "scope-5"}, 24), wantArgs: []any{"sessions", "workers", int64(50), []byte("key-000500"), "scope-5", 25}},
		// Storage v0.6.0 ListDue has no scope argument. Both pages must use the
		// namespace+state+due tuple index, proving the runbook shorthand was not
		// accidentally implemented as an API-incompatible scope filter.
		{name: "due first no invented scope", indexName: prefix + "ordered_due_idx", statement: orderedquery.Due(table, "sessions", 999, nil, 24), wantArgs: []any{"sessions", int64(999), 25}},
		{name: "due middle no invented scope", indexName: prefix + "ordered_due_idx", statement: orderedquery.Due(table, "sessions", 999, &orderedquery.DuePosition{DueAt: 500, StableKey: []byte("key-000500"), OrderingScope: "scope-5"}, 24), wantArgs: []any{"sessions", int64(999), int64(500), []byte("key-000500"), "scope-5", 25}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if !reflect.DeepEqual(test.statement.Args, test.wantArgs) {
				t.Fatalf("production query arguments = %#v, want exact values/types/order %#v", test.statement.Args, test.wantArgs)
			}
			var raw []byte
			if err := tx.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+test.statement.SQL, test.statement.Args...).Scan(&raw); err != nil {
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
