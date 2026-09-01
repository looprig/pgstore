//go:build integration

package orderedindex

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/looprig/storage"
)

var integrationSchemaID atomic.Uint64

func newIntegrationStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("PGSTORE_TEST_DSN")
	if dsn == "" {
		t.Skip("PGSTORE_TEST_DSN is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	schema := fmt.Sprintf("orderedfault%x", integrationSchemaID.Add(1))
	_, err = pool.Exec(ctx, `CREATE SCHEMA `+schema+`;
CREATE TABLE `+schema+`.test_ordered_scopes (
 namespace text COLLATE "C" NOT NULL, ordering_scope text COLLATE "C" NOT NULL,
 next_order numeric(20,0) NOT NULL CHECK (next_order >= 0 AND next_order <= 18446744073709551615),
 PRIMARY KEY (namespace, ordering_scope));
CREATE TABLE `+schema+`.test_ordered_records (
 namespace text COLLATE "C" NOT NULL, ordering_scope text COLLATE "C" NOT NULL,
 stable_key text COLLATE "C" NOT NULL, ranking_scope text COLLATE "C" NOT NULL,
 revision numeric(20,0) NOT NULL, order_id numeric(20,0) NOT NULL,
 value bytea NOT NULL, value_is_nil boolean NOT NULL, ranked boolean NOT NULL,
 rank_value bigint NOT NULL, due_state smallint NOT NULL, due_at bigint NOT NULL,
 deleted boolean NOT NULL, PRIMARY KEY(namespace, ordering_scope, stable_key));`)
	if err != nil {
		pool.Close()
		t.Fatalf("schema: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if _, err := pool.Exec(cleanupCtx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("cleanup schema: %v", err)
		}
		pool.Close()
	})
	return New(pool, schema, "test_"), pool
}

func orderedID(key string) storage.OrderedID {
	return storage.OrderedID{Namespace: "sessions", OrderingScope: "acceptance", StableKey: storage.StableKey(key)}
}

func boundedContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestCommitAcknowledgementLossResolvesOnlyExactPostState(t *testing.T) {
	store, pool := newIntegrationStore(t)
	ctx := boundedContext(t)
	lost := errors.New("injected acknowledgement loss")
	commitThenLose := func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		return lost
	}

	store.commit = commitThenLose
	created, won, err := store.Create(ctx, orderedID("create-exact"), "workers", []byte("value"), storage.Rank{Ranked: true, Value: 1}, storage.Due{State: storage.DueAt, UnixMillis: 1})
	if err != nil || !won || created.Revision != 1 {
		t.Fatalf("Create exact committed state = %#v, %v, %v", created, won, err)
	}

	store.commit = func(ctx context.Context, tx pgx.Tx) error { return tx.Commit(ctx) }
	updateBase, _, err := store.Create(ctx, orderedID("update-exact"), "workers", []byte("before"), storage.Rank{}, storage.Due{State: storage.NotDue})
	if err != nil {
		t.Fatalf("seed update: %v", err)
	}
	store.commit = commitThenLose
	updated, err := store.Update(ctx, updateBase.ID, updateBase.Revision, []byte("after"), storage.Rank{Ranked: true, Value: 9}, storage.Due{State: storage.DueAt, UnixMillis: 9})
	if err != nil || updated.Revision != 2 {
		t.Fatalf("Update exact committed state = %#v, %v", updated, err)
	}

	store.commit = func(ctx context.Context, tx pgx.Tx) error { return tx.Commit(ctx) }
	deleteBase, _, err := store.Create(ctx, orderedID("delete-exact"), "workers", []byte("before"), storage.Rank{Ranked: true, Value: 1}, storage.Due{State: storage.DueAt, UnixMillis: 1})
	if err != nil {
		t.Fatalf("seed delete: %v", err)
	}
	store.commit = commitThenLose
	deleted, err := store.Delete(ctx, deleteBase.ID, deleteBase.Revision)
	if err != nil || !deleted.Deleted || deleted.Revision != 2 {
		t.Fatalf("Delete exact committed state = %#v, %v", deleted, err)
	}

	store.commit = func(ctx context.Context, tx pgx.Tx) error { return tx.Commit(ctx) }
	laterBase, _, err := store.Create(ctx, orderedID("later-update"), "workers", []byte("before"), storage.Rank{}, storage.Due{State: storage.NotDue})
	if err != nil {
		t.Fatalf("seed later update: %v", err)
	}
	store.commit = func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		_, execErr := pool.Exec(ctx, `UPDATE `+store.recordsTable()+` SET revision = revision + 1, value = 'later'::bytea WHERE namespace = $1 AND ordering_scope = $2 AND stable_key = $3`, laterBase.ID.Namespace, laterBase.ID.OrderingScope, string(laterBase.ID.StableKey))
		if execErr != nil {
			return execErr
		}
		return lost
	}
	_, err = store.Update(ctx, laterBase.ID, laterBase.Revision, []byte("committed"), storage.Rank{}, storage.Due{State: storage.NotDue})
	var ambiguous *storage.OrderedAmbiguousError
	if !errors.As(err, &ambiguous) || ambiguous.Operation != storage.OrderedUpdateOperation || ambiguous.Cause == nil {
		t.Fatalf("Update followed by a later revision = %T %v, want update ambiguity with a safe cause", err, err)
	}
}

func TestCanceledOperationsNeverReachPostgreSQLRetry(t *testing.T) {
	store, _ := newIntegrationStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	cancel()
	id := orderedID("canceled")
	operations := []func() error{
		func() error { _, err := store.Get(ctx, id); return err },
		func() error {
			_, _, err := store.Create(ctx, id, "workers", nil, storage.Rank{}, storage.Due{State: storage.NotDue})
			return err
		},
		func() error {
			_, err := store.Update(ctx, id, 1, nil, storage.Rank{}, storage.Due{State: storage.NotDue})
			return err
		},
		func() error { _, err := store.Delete(ctx, id, 1); return err },
		func() error { _, err := store.ListOrdered(ctx, "sessions", "acceptance", 0, 1); return err },
		func() error { _, err := store.ListRanked(ctx, "sessions", "workers", "", 1); return err },
		func() error { _, err := store.ListDue(ctx, "sessions", 0, "", 1); return err },
	}
	for index, operation := range operations {
		if err := operation(); !errors.Is(err, context.Canceled) {
			t.Errorf("operation %d = %T %v, want context.Canceled", index, err, err)
		}
	}
}

func TestRevisionAndOrderExhaustionLeaveAuthoritativeStateUntouched(t *testing.T) {
	store, pool := newIntegrationStore(t)
	ctx := boundedContext(t)
	id := orderedID("revision-max")
	created, _, err := store.Create(ctx, id, "workers", []byte("original"), storage.Rank{Ranked: true, Value: 1}, storage.Due{State: storage.DueAt, UnixMillis: 1})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := pool.Exec(ctx, "UPDATE "+store.recordsTable()+" SET revision=$4::numeric WHERE namespace=$1 AND ordering_scope=$2 AND stable_key=$3", id.Namespace, id.OrderingScope, string(id.StableKey), fmt.Sprint(uint64(math.MaxUint64))); err != nil {
		t.Fatalf("seed maximal revision: %v", err)
	}
	for name, operation := range map[string]func() error{
		"Update": func() error {
			_, err := store.Update(ctx, id, math.MaxUint64, []byte("changed"), storage.Rank{}, storage.Due{State: storage.NotDue})
			return err
		},
		"Delete": func() error { _, err := store.Delete(ctx, id, math.MaxUint64); return err },
	} {
		err := operation()
		var exhausted *storage.OrderedRevisionExhaustedError
		if !errors.As(err, &exhausted) || exhausted.ID != id || exhausted.Revision != math.MaxUint64 {
			t.Errorf("%s at maximal revision = %T %v, want typed exhaustion", name, err, err)
		}
	}
	got, err := store.Get(ctx, id)
	if err != nil || got.Revision != math.MaxUint64 || string(got.Value) != "original" || got.Deleted || got.Order != created.Order {
		t.Fatalf("record after exhausted mutations = %#v, %v", got, err)
	}

	if _, err := pool.Exec(ctx, "UPDATE "+store.scopesTable()+" SET next_order=$3::numeric WHERE namespace=$1 AND ordering_scope=$2", id.Namespace, id.OrderingScope, fmt.Sprint(uint64(math.MaxUint64))); err != nil {
		t.Fatalf("seed maximal order: %v", err)
	}
	_, won, err := store.Create(ctx, orderedID("order-max"), "workers", nil, storage.Rank{}, storage.Due{State: storage.NotDue})
	var orderExhausted *orderExhaustedError
	if won || !errors.As(err, &orderExhausted) {
		t.Fatalf("Create at maximal order = won %v, %T %v, want local exhaustion", won, err, err)
	}
	_, err = store.Get(ctx, orderedID("order-max"))
	var absent *storage.OrderedRecordNotFoundError
	if !errors.As(err, &absent) {
		t.Fatalf("Get after exhausted Create = %T %v, want not found", err, err)
	}
}

func TestNilAndEmptyValuesRemainDistinctCallerOwnedSnapshots(t *testing.T) {
	store, _ := newIntegrationStore(t)
	ctx := boundedContext(t)
	nilRecord, _, err := store.Create(ctx, orderedID("nil-value"), "workers", nil, storage.Rank{}, storage.Due{State: storage.NotDue})
	if err != nil || nilRecord.Value != nil {
		t.Fatalf("Create nil value = %#v, %v", nilRecord.Value, err)
	}
	emptyRecord, _, err := store.Create(ctx, orderedID("empty-value"), "workers", []byte{}, storage.Rank{}, storage.Due{State: storage.NotDue})
	if err != nil || emptyRecord.Value == nil || len(emptyRecord.Value) != 0 {
		t.Fatalf("Create empty value = %#v, %v", emptyRecord.Value, err)
	}
	for _, want := range []storage.OrderedRecord{nilRecord, emptyRecord} {
		got, err := store.Get(ctx, want.ID)
		if err != nil || (got.Value == nil) != (want.Value == nil) {
			t.Errorf("Get(%s) nil=%v, err %v; want nil=%v", want.ID.StableKey, got.Value == nil, err, want.Value == nil)
		}
	}
}

func TestConcurrentDuplicateConsumesExactlyOneCounterValue(t *testing.T) {
	store, pool := newIntegrationStore(t)
	ctx := boundedContext(t)
	id := orderedID("contended-counter")
	const writers = 32
	start := make(chan struct{})
	type result struct {
		record  storage.OrderedRecord
		created bool
		err     error
	}
	results := make(chan result, writers)
	for range writers {
		go func() {
			<-start
			record, created, err := store.Create(ctx, id, "workers", []byte("value"), storage.Rank{}, storage.Due{State: storage.NotDue})
			results <- result{record: record, created: created, err: err}
		}()
	}
	close(start)
	winners := 0
	for range writers {
		result := <-results
		if result.err != nil {
			t.Errorf("concurrent duplicate: %v", result.err)
		}
		if result.created {
			winners++
		}
		if result.record.ID != id || result.record.Order != 1 || result.record.Revision != 1 {
			t.Errorf("concurrent duplicate returned %#v, want canonical order/revision 1", result.record)
		}
	}
	if winners != 1 {
		t.Errorf("concurrent duplicate winners = %d, want 1", winners)
	}
	var nextOrder uint64
	if err := pool.QueryRow(ctx, "SELECT next_order FROM "+store.scopesTable()+" WHERE namespace=$1 AND ordering_scope=$2", id.Namespace, id.OrderingScope).Scan(&nextOrder); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	if nextOrder != 1 {
		t.Fatalf("counter after duplicate race = %d, want exactly 1", nextOrder)
	}
}

func TestListViewsIsolateNamespaceAndOrderingScope(t *testing.T) {
	store, _ := newIntegrationStore(t)
	ctx := boundedContext(t)
	ids := []storage.OrderedID{
		{Namespace: "sessions", OrderingScope: "acceptance", StableKey: "shared"},
		{Namespace: "other", OrderingScope: "acceptance", StableKey: "shared"},
		{Namespace: "sessions", OrderingScope: "other-scope", StableKey: "shared"},
	}
	for _, id := range ids {
		if _, _, err := store.Create(ctx, id, "workers", []byte(id.Namespace+"/"+id.OrderingScope), storage.Rank{Ranked: true, Value: 1}, storage.Due{State: storage.DueAt, UnixMillis: 1}); err != nil {
			t.Fatalf("Create(%#v): %v", id, err)
		}
	}
	ordered, err := store.ListOrdered(ctx, "sessions", "acceptance", 0, 10)
	if err != nil || len(ordered.Records) != 1 || ordered.Records[0].ID != ids[0] {
		t.Fatalf("ListOrdered isolation = %#v, %v", ordered.Records, err)
	}
	ranked, err := store.ListRanked(ctx, "sessions", "workers", "", 10)
	if err != nil || len(ranked.Records) != 2 {
		t.Fatalf("ListRanked namespace isolation = %#v, %v", ranked.Records, err)
	}
	due, err := store.ListDue(ctx, "sessions", 1, "", 10)
	if err != nil || len(due.Records) != 2 {
		t.Fatalf("ListDue namespace isolation = %#v, %v", due.Records, err)
	}
	for _, record := range append(ranked.Records, due.Records...) {
		if record.ID.Namespace != "sessions" {
			t.Errorf("view leaked namespace: %#v", record.ID)
		}
	}
}
