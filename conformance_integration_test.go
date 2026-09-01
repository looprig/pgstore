//go:build integration

package pgstore

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looprig/storage"
	"github.com/looprig/storage/storetest"
)

var conformanceStoreID atomic.Uint64

func newConformanceStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("PGSTORE_TEST_DSN")
	if dsn == "" {
		t.Skip("PGSTORE_TEST_DSN is not set; P1.2 owns disposable PostgreSQL provisioning")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	prefix := fmt.Sprintf("test%x_%x_", time.Now().UnixNano(), conformanceStoreID.Add(1))
	store, err := Open(ctx, Options{
		DSN:                        dsn,
		TablePrefix:                prefix,
		Migrations:                 MigrationApply,
		AllowInsecureLocalhostOnly: true,
	})
	if err != nil {
		t.Fatalf("Open conformance store: %v", err)
	}
	t.Cleanup(store.Close)
	return store
}

func TestLedgerConformance(t *testing.T) {
	storetest.TestLedger(t, func(t *testing.T) storage.Ledger {
		return newConformanceStore(t).Ledger
	})
}

func TestKVConformance(t *testing.T) {
	storetest.TestKV(t, func(t *testing.T) storage.KV {
		return newConformanceStore(t).KV
	})
}

func TestLeaserConformance(t *testing.T) {
	storetest.TestLeaser(t, func(t *testing.T) storage.Leaser {
		return newConformanceStore(t).Leaser
	})
}

func TestLeaserLifecycleConformance(t *testing.T) {
	storetest.TestLeaserLifecycle(t, func(t *testing.T) storetest.LeaserLifecycleHarness {
		dsn := os.Getenv("PGSTORE_TEST_DSN")
		if dsn == "" {
			t.Skip("PGSTORE_TEST_DSN is not set")
		}
		const ttl = 500 * time.Millisecond
		const renewInterval = 100 * time.Millisecond
		prefix := fmt.Sprintf("lifecycle%x_%x_", time.Now().UnixNano(), conformanceStoreID.Add(1))
		open := func(t *testing.T, mode MigrationMode) *Store {
			t.Helper()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			store, err := Open(ctx, Options{
				DSN: dsn, TablePrefix: prefix, Migrations: mode,
				LeaseTTL: ttl, LeaseRenewInterval: renewInterval,
				AllowInsecureLocalhostOnly: true,
			})
			if err != nil {
				t.Fatalf("Open lifecycle store: %v", err)
			}
			t.Cleanup(store.Close)
			return store
		}
		primary := open(t, MigrationApply)
		admin := adminPool(t)
		table := `"looprig"."` + prefix + `leases"`
		var viewID atomic.Uint64
		return storetest.LeaserLifecycleHarness{
			Primary:       primary.Leaser,
			PrimaryViewID: viewID.Add(1),
			OpenIndependent: func(t *testing.T) storetest.LeaserLifecycleClient {
				return storetest.LeaserLifecycleClient{Leaser: open(t, MigrationValidate).Leaser, ViewID: viewID.Add(1)}
			},
			Renew: func(t *testing.T, lease storage.Lease) {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				command, err := admin.Exec(ctx, "UPDATE "+table+" SET expires_at = clock_timestamp() + $2::bigint * interval '1 millisecond', revision = revision + 1 WHERE epoch = $1 AND expires_at > clock_timestamp()", int64(lease.Epoch()), ttl.Milliseconds())
				if err != nil || command.RowsAffected() != 1 {
					t.Fatalf("deterministic Renew = rows %d, %v", command.RowsAffected(), err)
				}
			},
			Expire: func(t *testing.T, lease storage.Lease) {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				command, err := admin.Exec(ctx, "UPDATE "+table+" SET expires_at = clock_timestamp() WHERE epoch = $1", int64(lease.Epoch()))
				if err != nil || command.RowsAffected() != 1 {
					t.Fatalf("deterministic Expire = rows %d, %v", command.RowsAffected(), err)
				}
				select {
				case <-lease.Lost():
				case <-ctx.Done():
					t.Fatal("Lost remained open after deterministic expiry")
				}
			},
		}
	})
}

type orderedCursorProbe struct{}

func (orderedCursorProbe) MalformedCursor(*testing.T, storage.OrderedCursorKind) string {
	return "pgstore-malformed"
}

func (orderedCursorProbe) UnknownVersionCursor(*testing.T, storage.OrderedCursorKind) string {
	return "pgo999:unknown-version"
}

func TestOrderedIndexConformance(t *testing.T) {
	t.Skip("P1.4 implements PostgreSQL ordered index")
	storetest.TestOrderedIndex(t, func(t *testing.T) storage.OrderedIndex {
		return newConformanceStore(t).OrderedIndex
	}, orderedCursorProbe{})
}
