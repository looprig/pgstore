// Package kv contains the PostgreSQL revision-CAS KV adapter seam.
package kv

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

func (s *Store) Get(ctx context.Context, _ string) ([]byte, uint64, error) {
	if err := guard.RequireDeadline(ctx, "KV.Get"); err != nil {
		return nil, 0, err
	}
	return nil, 0, guard.NotImplemented("KV.Get")
}

func (s *Store) Put(ctx context.Context, _ string, _ uint64, _ []byte) (uint64, error) {
	if err := guard.RequireDeadline(ctx, "KV.Put"); err != nil {
		return 0, err
	}
	return 0, guard.NotImplemented("KV.Put")
}

func (s *Store) Keys(ctx context.Context, _ string) ([]string, error) {
	if err := guard.RequireDeadline(ctx, "KV.Keys"); err != nil {
		return nil, err
	}
	return nil, guard.NotImplemented("KV.Keys")
}

func (s *Store) Delete(ctx context.Context, _ string) error {
	if err := guard.RequireDeadline(ctx, "KV.Delete"); err != nil {
		return err
	}
	return guard.NotImplemented("KV.Delete")
}

var _ storage.KV = (*Store)(nil)
