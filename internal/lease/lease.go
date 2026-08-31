// Package lease contains the PostgreSQL renewable epoch-lease adapter seam.
package lease

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/looprig/pgstore/internal/guard"
	"github.com/looprig/storage"
)

type Store struct {
	pool        *pgxpool.Pool
	schema      string
	tablePrefix string
}

func New(pool *pgxpool.Pool, schema, tablePrefix string) *Store {
	return &Store{pool: pool, schema: schema, tablePrefix: tablePrefix}
}

func (s *Store) Acquire(ctx context.Context, _ string) (storage.Lease, error) {
	if err := guard.RequireDeadline(ctx, "Leaser.Acquire"); err != nil {
		return nil, err
	}
	return nil, guard.NotImplemented("Leaser.Acquire")
}

var _ storage.Leaser = (*Store)(nil)
