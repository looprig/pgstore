// Package ledger contains the PostgreSQL Ledger adapter seam.
package ledger

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/looprig/pgstore/internal/guard"
	"github.com/looprig/storage"
)

// Store is the P1.1 Ledger adapter. P1.2 replaces its honest scaffold returns
// with transactional PostgreSQL operations without changing public wiring.
type Store struct {
	pool        *pgxpool.Pool
	schema      string
	tablePrefix string
}

func New(pool *pgxpool.Pool, schema, tablePrefix string) *Store {
	return &Store{pool: pool, schema: schema, tablePrefix: tablePrefix}
}

func (s *Store) Append(ctx context.Context, _ string, _ uint64, _ []byte) error {
	if err := guard.RequireDeadline(ctx, "Ledger.Append"); err != nil {
		return err
	}
	return guard.NotImplemented("Ledger.Append")
}

func (s *Store) Read(ctx context.Context, _ string, _ uint64) (storage.Cursor, error) {
	if err := guard.RequireDeadline(ctx, "Ledger.Read"); err != nil {
		return nil, err
	}
	return nil, guard.NotImplemented("Ledger.Read")
}

func (s *Store) Tip(ctx context.Context, _ string) (uint64, error) {
	if err := guard.RequireDeadline(ctx, "Ledger.Tip"); err != nil {
		return 0, err
	}
	return 0, guard.NotImplemented("Ledger.Tip")
}

func (s *Store) Delete(ctx context.Context, _ string) error {
	if err := guard.RequireDeadline(ctx, "Ledger.Delete"); err != nil {
		return err
	}
	return guard.NotImplemented("Ledger.Delete")
}

var _ storage.Ledger = (*Store)(nil)
