//go:build integration

package pgstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/looprig/storage"
)

// TestLeaseReleaseIsBoundedByTheDefault witnesses Lease.Release's default
// bound, which a hung server cannot reach (Release needs a granted lease).
// Another transaction holds the lease row FOR UPDATE, and LockTimeout is far
// above the bound, so only the default can end the deadline-free Release.
func TestLeaseReleaseIsBoundedByTheDefault(t *testing.T) {
	dsn := os.Getenv("PGSTORE_TEST_DSN")
	if dsn == "" {
		t.Skip("PGSTORE_TEST_DSN is not set")
	}
	const bound = 300 * time.Millisecond
	prefix := fmt.Sprintf("relbound%x_", time.Now().UnixNano())
	openCtx, cancelOpen := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelOpen()
	store, err := Open(openCtx, Options{
		DSN: dsn, TablePrefix: prefix, Migrations: MigrationApply, AllowInsecureLocalhostOnly: true,
		StatementTimeout: time.Minute, LockTimeout: 30 * time.Second,
		LeaseTTL: time.Minute, LeaseRenewInterval: 20 * time.Second,
		DefaultOperationTimeout: bound,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(store.Close)
	lease, err := store.Leaser.Acquire(context.Background(), "leases/held")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	admin, err := pgxpool.New(openCtx, dsn)
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	t.Cleanup(admin.Close)
	blocker, err := admin.Begin(openCtx)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	defer func() { _ = blocker.Rollback(context.Background()) }()
	if _, err := blocker.Exec(openCtx, `SELECT 1 FROM "looprig".`+`"`+prefix+`leases" WHERE name = $1 FOR UPDATE`, "leases/held"); err != nil {
		t.Fatalf("lock lease row: %v", err)
	}

	started := time.Now()
	done := make(chan error, 1)
	go func() { done <- lease.Release(context.Background()) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Release error = %T %v, want context.DeadlineExceeded", err, err)
		}
		if elapsed := time.Since(started); elapsed < bound/2 {
			t.Fatalf("Release returned after %v, before the %v bound", elapsed, bound)
		}
	case <-time.After(bound + 5*time.Second):
		t.Fatal("Release did not return within the default bound")
	}
}

// opaqueContext is a cancelable context the context package cannot see
// through, so context.WithTimeout keeps one goroutine per child until that
// child is cancelled: a default bound whose cancel is dropped is countable
// long before its timer fires.
type opaqueContext struct {
	done chan struct{}
	once sync.Once
}

func (*opaqueContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *opaqueContext) Done() <-chan struct{}     { return c.done }
func (*opaqueContext) Value(any) any               { return nil }
func (c *opaqueContext) cancel()                   { c.once.Do(func() { close(c.done) }) }
func (c *opaqueContext) Err() error {
	select {
	case <-c.done:
		return context.Canceled
	default:
		return nil
	}
}

// TestDefaultBoundIsReleasedAfterEveryOperation witnesses that every
// deadline-free operation cancels its default bound when it returns.
func TestDefaultBoundIsReleasedAfterEveryOperation(t *testing.T) {
	store := newUndatedStore(t)
	parent := &opaqueContext{done: make(chan struct{})}
	t.Cleanup(parent.cancel)
	var ctx context.Context = parent
	round := func(i int) {
		t.Helper()
		id := storage.OrderedID{Namespace: "ns", OrderingScope: "scope", StableKey: storage.StableKey(fmt.Sprintf("key-%d", i))}
		key := fmt.Sprintf("kv/%d", i)
		if _, err := store.KV.Put(ctx, key, 0, []byte("v")); err != nil {
			t.Fatalf("KV.Put: %v", err)
		}
		if _, _, err := store.KV.Get(ctx, key); err != nil {
			t.Fatalf("KV.Get: %v", err)
		}
		if _, err := store.KV.Keys(ctx, "kv/"); err != nil {
			t.Fatalf("KV.Keys: %v", err)
		}
		if err := store.KV.Delete(ctx, key); err != nil {
			t.Fatalf("KV.Delete: %v", err)
		}
		ledger := fmt.Sprintf("ledger/%d", i)
		if err := store.Ledger.Append(ctx, ledger, 0, []byte("r")); err != nil {
			t.Fatalf("Ledger.Append: %v", err)
		}
		cursor, err := store.Ledger.Read(ctx, ledger, 1)
		if err != nil {
			t.Fatalf("Ledger.Read: %v", err)
		}
		if _, err := cursor.Next(ctx); err != nil {
			t.Fatalf("Cursor.Next: %v", err)
		}
		_ = cursor.Close()
		if _, err := store.Ledger.Tip(ctx, ledger); err != nil {
			t.Fatalf("Ledger.Tip: %v", err)
		}
		if err := store.Ledger.Delete(ctx, ledger); err != nil {
			t.Fatalf("Ledger.Delete: %v", err)
		}
		lease, err := store.Leaser.Acquire(ctx, fmt.Sprintf("leases/%d", i))
		if err != nil {
			t.Fatalf("Leaser.Acquire: %v", err)
		}
		if err := lease.Release(ctx); err != nil {
			t.Fatalf("Lease.Release: %v", err)
		}
		record, created, err := store.OrderedIndex.Create(ctx, id, "rank", []byte("v"), storage.Rank{}, storage.Due{})
		if err != nil || !created {
			t.Fatalf("OrderedIndex.Create = (%v, %v)", created, err)
		}
		if _, err := store.OrderedIndex.Get(ctx, id); err != nil {
			t.Fatalf("OrderedIndex.Get: %v", err)
		}
		record, err = store.OrderedIndex.Update(ctx, id, record.Revision, []byte("w"), storage.Rank{}, storage.Due{})
		if err != nil {
			t.Fatalf("OrderedIndex.Update: %v", err)
		}
		if _, err := store.OrderedIndex.ListOrdered(ctx, "ns", "scope", 0, 10); err != nil {
			t.Fatalf("ListOrdered: %v", err)
		}
		if _, err := store.OrderedIndex.ListRanked(ctx, "ns", "rank", "", 10); err != nil {
			t.Fatalf("ListRanked: %v", err)
		}
		if _, err := store.OrderedIndex.ListDue(ctx, "ns", 0, "", 10); err != nil {
			t.Fatalf("ListDue: %v", err)
		}
		if _, err := store.OrderedIndex.Delete(ctx, id, record.Revision); err != nil {
			t.Fatalf("OrderedIndex.Delete: %v", err)
		}
	}

	round(1000)
	baseline := settledGoroutines()
	const rounds = 40
	for i := 0; i < rounds; i++ {
		round(i)
	}
	if after := settledGoroutines(); after > baseline+rounds/2 {
		t.Fatalf("goroutines %d -> %d after %d rounds: a default bound was not released", baseline, after, rounds)
	}
}

func settledGoroutines() int {
	count := runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		time.Sleep(10 * time.Millisecond)
		next := runtime.NumGoroutine()
		if next == count {
			return next
		}
		count = next
	}
	return count
}
