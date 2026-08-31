// Package pgstore provides PostgreSQL-backed structured Storage primitives.
// Blobs are intentionally absent: cloud composition supplies them from s3store.
package pgstore

import (
	"context"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/looprig/pgstore/internal/guard"
	"github.com/looprig/pgstore/internal/kv"
	"github.com/looprig/pgstore/internal/lease"
	"github.com/looprig/pgstore/internal/ledger"
	"github.com/looprig/pgstore/internal/orderedindex"
	"github.com/looprig/storage"
)

type DeadlineRequiredError = guard.DeadlineRequiredError
type NotImplementedError = guard.NotImplementedError

// Store bundles the four structured primitives. It deliberately is not a
// storage.Composite, whose constructor requires a Blobs implementation.
type Store struct {
	Ledger       storage.Ledger
	Leaser       storage.Leaser
	KV           storage.KV
	OrderedIndex storage.OrderedIndex

	pool      *pgxpool.Pool
	closeOnce sync.Once
	closePool func()
}

var newPool = pgxpool.NewWithConfig

// Open validates options, requires a caller deadline, creates a lazy pgx pool,
// and wires the P1.1 adapter seams. It does not ping or migrate PostgreSQL;
// disposable database setup and the first schema belong to P1.2.
func Open(ctx context.Context, options Options) (*Store, error) {
	resolved, err := options.resolve()
	if err != nil {
		return nil, err
	}
	if err := guard.RequireDeadline(ctx, "Open"); err != nil {
		return nil, err
	}
	pool, err := newPool(ctx, resolved.poolConfig)
	if err != nil {
		// NewWithConfig validates already-parsed configuration. Do not expose its
		// text because driver errors may retain connection details.
		return nil, invalidOption("DSN", "PostgreSQL pool configuration was rejected")
	}
	return &Store{
		Ledger:       ledger.New(pool, resolved.schema, resolved.tablePrefix),
		Leaser:       lease.New(pool, resolved.schema, resolved.tablePrefix),
		KV:           kv.New(pool, resolved.schema, resolved.tablePrefix),
		OrderedIndex: orderedindex.New(pool, resolved.schema, resolved.tablePrefix),
		pool:         pool,
		closePool:    pool.Close,
	}, nil
}

// Close releases the lazy pool and is safe to call repeatedly.
func (s *Store) Close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(s.closePool)
}
