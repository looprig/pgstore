// Package orderedindex contains the bounded PostgreSQL OrderedIndex adapter seam.
package orderedindex

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

func (s *Store) Get(ctx context.Context, _ storage.OrderedID) (storage.OrderedRecord, error) {
	if err := guard.RequireDeadline(ctx, "OrderedIndex.Get"); err != nil {
		return storage.OrderedRecord{}, err
	}
	return storage.OrderedRecord{}, guard.NotImplemented("OrderedIndex.Get")
}

func (s *Store) Create(ctx context.Context, _ storage.OrderedID, _ string, _ []byte, _ storage.Rank, _ storage.Due) (storage.OrderedRecord, bool, error) {
	if err := guard.RequireDeadline(ctx, "OrderedIndex.Create"); err != nil {
		return storage.OrderedRecord{}, false, err
	}
	return storage.OrderedRecord{}, false, guard.NotImplemented("OrderedIndex.Create")
}

func (s *Store) Update(ctx context.Context, _ storage.OrderedID, _ uint64, _ []byte, _ storage.Rank, _ storage.Due) (storage.OrderedRecord, error) {
	if err := guard.RequireDeadline(ctx, "OrderedIndex.Update"); err != nil {
		return storage.OrderedRecord{}, err
	}
	return storage.OrderedRecord{}, guard.NotImplemented("OrderedIndex.Update")
}

func (s *Store) Delete(ctx context.Context, _ storage.OrderedID, _ uint64) (storage.OrderedRecord, error) {
	if err := guard.RequireDeadline(ctx, "OrderedIndex.Delete"); err != nil {
		return storage.OrderedRecord{}, err
	}
	return storage.OrderedRecord{}, guard.NotImplemented("OrderedIndex.Delete")
}

func (s *Store) ListOrdered(ctx context.Context, _ string, _ string, _ uint64, _ int) (storage.OrderedPage, error) {
	if err := guard.RequireDeadline(ctx, "OrderedIndex.ListOrdered"); err != nil {
		return storage.OrderedPage{}, err
	}
	return storage.OrderedPage{}, guard.NotImplemented("OrderedIndex.ListOrdered")
}

func (s *Store) ListRanked(ctx context.Context, _ string, _ string, _ storage.RankedCursor, _ int) (storage.RankedPage, error) {
	if err := guard.RequireDeadline(ctx, "OrderedIndex.ListRanked"); err != nil {
		return storage.RankedPage{}, err
	}
	return storage.RankedPage{}, guard.NotImplemented("OrderedIndex.ListRanked")
}

func (s *Store) ListDue(ctx context.Context, _ string, _ int64, _ storage.DueCursor, _ int) (storage.DuePage, error) {
	if err := guard.RequireDeadline(ctx, "OrderedIndex.ListDue"); err != nil {
		return storage.DuePage{}, err
	}
	return storage.DuePage{}, guard.NotImplemented("OrderedIndex.ListDue")
}

var _ storage.OrderedIndex = (*Store)(nil)
