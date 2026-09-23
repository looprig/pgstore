// Package kv contains the PostgreSQL revision-CAS KV adapter.
package kv

import (
	"bytes"
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/looprig/pgstore/internal/guard"
	pginternal "github.com/looprig/pgstore/internal/postgres"
	"github.com/looprig/storage"
)

type Store struct {
	operationTimeout    time.Duration
	pool                *pgxpool.Pool
	schema, tablePrefix string
	put                 func(context.Context, string, uint64, []byte) (uint64, error)
	delete              func(context.Context, string) error
}

func New(pool *pgxpool.Pool, schema, tablePrefix string) *Store {
	store := &Store{operationTimeout: guard.DefaultOperationTimeout, pool: pool, schema: schema, tablePrefix: tablePrefix}
	store.put = store.putOnce
	store.delete = store.deleteOnce
	return store
}

// WithOperationTimeout sets the bound applied to an operation whose context
// has no deadline. It must be called before the Store is shared.
func (s *Store) WithOperationTimeout(timeout time.Duration) *Store {
	s.operationTimeout = timeout
	return s
}

func (s *Store) Get(ctx context.Context, key string) ([]byte, uint64, error) {
	ctx, cancel, err := guard.Bound(ctx, "KV.Get", s.operationTimeout)
	if err != nil {
		return nil, 0, err
	}
	defer cancel()
	if err := storage.ValidateName(key); err != nil {
		return nil, 0, err
	}
	var value []byte
	var revision uint64
	err = s.pool.QueryRow(ctx, "SELECT value, revision FROM "+s.table()+" WHERE key = $1", key).Scan(&value, &revision)
	if err == pgx.ErrNoRows {
		return nil, 0, &storage.KeyNotFoundError{Key: key}
	}
	if err != nil {
		return nil, 0, failure(ctx, "kv get")
	}
	return bytes.Clone(value), revision, nil
}

func (s *Store) Put(ctx context.Context, key string, expected uint64, value []byte) (uint64, error) {
	ctx, cancel, err := guard.Bound(ctx, "KV.Put", s.operationTimeout)
	if err != nil {
		return 0, err
	}
	defer cancel()
	if err := storage.ValidateName(key); err != nil {
		return 0, err
	}
	revision, err := s.put(ctx, key, expected, value)
	if err == pgx.ErrNoRows {
		return 0, &storage.ConflictError{Name: key, Expected: expected}
	}
	if err != nil {
		return s.resolvePut(key, expected, value, safeCause(ctx, "kv put"))
	}
	return revision, nil
}

func (s *Store) putOnce(ctx context.Context, key string, expected uint64, value []byte) (uint64, error) {
	if value == nil {
		value = []byte{}
	}
	var revision uint64
	var err error
	if expected == 0 {
		err = s.pool.QueryRow(ctx, "INSERT INTO "+s.table()+" (key, revision, value) VALUES ($1, 1, $2) ON CONFLICT (key) DO NOTHING RETURNING revision", key, value).Scan(&revision)
	} else {
		err = s.pool.QueryRow(ctx, "UPDATE "+s.table()+" SET revision = revision + 1, value = $3 WHERE key = $1 AND revision = $2 RETURNING revision", key, expected, value).Scan(&revision)
	}
	return revision, err
}

func (s *Store) Keys(ctx context.Context, prefix string) ([]string, error) {
	ctx, cancel, err := guard.Bound(ctx, "KV.Keys", s.operationTimeout)
	if err != nil {
		return nil, err
	}
	defer cancel()
	rows, err := s.pool.Query(ctx, "SELECT key FROM "+s.table()+" WHERE left(key, length($1)) = $1 ORDER BY key COLLATE \"C\"", prefix)
	if err != nil {
		return nil, failure(ctx, "kv keys")
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, pginternal.RedactedError("kv keys")
		}
		keys = append(keys, key)
	}
	if rows.Err() != nil {
		return nil, failure(ctx, "kv keys")
	}
	return keys, nil
}

func (s *Store) Delete(ctx context.Context, key string) error {
	ctx, cancel, err := guard.Bound(ctx, "KV.Delete", s.operationTimeout)
	if err != nil {
		return err
	}
	defer cancel()
	if err := storage.ValidateName(key); err != nil {
		return err
	}
	if err := s.delete(ctx, key); err != nil {
		return s.resolveDelete(key, safeCause(ctx, "kv delete"))
	}
	return nil
}

func (s *Store) deleteOnce(ctx context.Context, key string) error {
	_, err := s.pool.Exec(ctx, "DELETE FROM "+s.table()+" WHERE key = $1", key)
	return err
}

func (s *Store) resolveDelete(key string, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), pginternal.AuthoritativeReadTimeout())
	defer cancel()
	var present int
	err := s.pool.QueryRow(ctx, "SELECT 1 FROM "+s.table()+" WHERE key = $1", key).Scan(&present)
	if err == pgx.ErrNoRows {
		return nil
	}
	if err == nil {
		return cause
	}
	return pginternal.RedactedError("kv delete outcome resolution")
}

func safeCause(ctx context.Context, operation string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return pginternal.RedactedError(operation)
}

func (s *Store) table() string { return pginternal.Qualified(s.schema, s.tablePrefix+"kv") }

// resolvePut uses a fresh bounded context because the caller's canceled context
// cannot answer whether PostgreSQL committed before its acknowledgement was lost.
//
// An absent key resolves to the original cause — a definite-failure signal such
// as context.Canceled — while an absent Ledger record resolves to
// storage.AmbiguousError. This asymmetry is deliberate, and it is NOT derived
// from the Ledger's stated reasoning: "a concurrent delete means absence cannot
// prove the write never committed" is equally true here, since a committed Put
// that is then deleted also rereads as absent. Reporting a definite failure for
// that outcome is inaccurate, and it is kept anyway for a narrower reason.
//
// A KV revision is a private CAS token: a caller that retries reads the current
// value and revision and converges, whatever really happened. A Ledger tip is
// not private — it is the expected sequence every subsequent append and every
// downstream reader is written against — so a caller told "definitely did not
// append" that in fact did append is permanently desynchronized from the log.
// The asymmetry therefore lands the pessimistic answer where a wrong answer is
// unrecoverable and the simpler answer where a retry repairs it. Should KV
// callers ever derive shared state from a revision, this must become ambiguous
// too.
func (s *Store) resolvePut(key string, expected uint64, value []byte, cause error) (uint64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), pginternal.AuthoritativeReadTimeout())
	defer cancel()
	var stored []byte
	var revision uint64
	err := s.pool.QueryRow(ctx, "SELECT value, revision FROM "+s.table()+" WHERE key = $1", key).Scan(&stored, &revision)
	if err == nil && revision == expected+1 && bytes.Equal(stored, value) {
		return revision, nil
	}
	if err == pgx.ErrNoRows || (err == nil && revision == expected) {
		return 0, cause
	}
	if err == nil {
		return 0, &storage.ConflictError{Name: key, Expected: expected}
	}
	return 0, pginternal.RedactedError("kv put outcome resolution")
}

func failure(ctx context.Context, operation string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return pginternal.RedactedError(operation)
}

var _ storage.KV = (*Store)(nil)
