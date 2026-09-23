//go:build integration

package pgstore

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/looprig/storage"
	"github.com/looprig/storage/storetest"
)

// undated strips every deadline and cancellation from a caller context: the
// shape host v0.5.0 hands SessionStore (context.WithoutCancel), which pgstore
// v0.1.x refused outright.
func undated(ctx context.Context) context.Context { return context.WithoutCancel(ctx) }

type undatedKV struct{ kv storage.KV }

func (u undatedKV) Get(ctx context.Context, key string) ([]byte, uint64, error) {
	return u.kv.Get(undated(ctx), key)
}
func (u undatedKV) Put(ctx context.Context, key string, expected uint64, value []byte) (uint64, error) {
	return u.kv.Put(undated(ctx), key, expected, value)
}
func (u undatedKV) Keys(ctx context.Context, prefix string) ([]string, error) {
	return u.kv.Keys(undated(ctx), prefix)
}
func (u undatedKV) Delete(ctx context.Context, key string) error {
	return u.kv.Delete(undated(ctx), key)
}

type undatedLedger struct{ ledger storage.Ledger }

func (u undatedLedger) Append(ctx context.Context, name string, expected uint64, payload []byte) error {
	return u.ledger.Append(undated(ctx), name, expected, payload)
}
func (u undatedLedger) Read(ctx context.Context, name string, from uint64) (storage.Cursor, error) {
	cursor, err := u.ledger.Read(undated(ctx), name, from)
	if err != nil {
		return nil, err
	}
	return undatedCursor{cursor}, nil
}
func (u undatedLedger) Tip(ctx context.Context, name string) (uint64, error) {
	return u.ledger.Tip(undated(ctx), name)
}
func (u undatedLedger) Delete(ctx context.Context, name string) error {
	return u.ledger.Delete(undated(ctx), name)
}

type undatedCursor struct{ cursor storage.Cursor }

func (u undatedCursor) Next(ctx context.Context) (storage.Record, error) {
	return u.cursor.Next(undated(ctx))
}
func (u undatedCursor) Close() error { return u.cursor.Close() }

// newUndatedStore opens with no caller deadline at all.
func newUndatedStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("PGSTORE_TEST_DSN")
	if dsn == "" {
		t.Skip("PGSTORE_TEST_DSN is not set")
	}
	prefix := fmt.Sprintf("undated%x_", time.Now().UnixNano())
	store, err := Open(context.Background(), Options{
		DSN: dsn, TablePrefix: prefix, Migrations: MigrationApply, AllowInsecureLocalhostOnly: true,
	})
	if err != nil {
		t.Fatalf("Open without a deadline: %v", err)
	}
	t.Cleanup(store.Close)
	return store
}

func TestKVWithoutCallerDeadlines(t *testing.T) {
	storetest.TestKV(t, func(t *testing.T) storage.KV { return undatedKV{newUndatedStore(t).KV} })
}

func TestLedgerWithoutCallerDeadlines(t *testing.T) {
	storetest.TestLedger(t, func(t *testing.T) storage.Ledger { return undatedLedger{newUndatedStore(t).Ledger} })
}

func TestLeaseAndOrderedIndexWithoutCallerDeadlines(t *testing.T) {
	store := newUndatedStore(t)
	ctx := context.Background()
	lease, err := store.Leaser.Acquire(ctx, "leases/undated")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := lease.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
	id := storage.OrderedID{Namespace: "ns", OrderingScope: "scope", StableKey: "key"}
	if _, _, err := store.OrderedIndex.Create(ctx, id, "rank", []byte("v"), storage.Rank{}, storage.Due{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	record, err := store.OrderedIndex.Get(ctx, id)
	if err != nil || string(record.Value) != "v" {
		t.Fatalf("Get = (%+v, %v)", record, err)
	}
	if page, err := store.OrderedIndex.ListOrdered(ctx, "ns", "scope", 0, 10); err != nil || len(page.Records) != 1 {
		t.Fatalf("ListOrdered = (%+v, %v)", page, err)
	}
}
