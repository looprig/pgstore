//go:build integration

package pgstore

import (
	"context"
	"os"
	"strconv"
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
	store, err := Open(ctx, Options{
		DSN:                        dsn,
		TablePrefix:                "test" + strconv.FormatUint(conformanceStoreID.Add(1), 10) + "_",
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

type orderedCursorProbe struct{}

func (orderedCursorProbe) MalformedCursor(*testing.T, storage.OrderedCursorKind) string {
	return "pgstore-malformed"
}

func (orderedCursorProbe) UnknownVersionCursor(*testing.T, storage.OrderedCursorKind) string {
	return "pgo999:unknown-version"
}

func TestOrderedIndexConformance(t *testing.T) {
	storetest.TestOrderedIndex(t, func(t *testing.T) storage.OrderedIndex {
		return newConformanceStore(t).OrderedIndex
	}, orderedCursorProbe{})
}
