// Package ledger contains the PostgreSQL Ledger adapter.
package ledger

import (
	"bytes"
	"context"
	"errors"
	"io"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/looprig/pgstore/internal/guard"
	pginternal "github.com/looprig/pgstore/internal/postgres"
	"github.com/looprig/storage"
)

type Store struct {
	pool        *pgxpool.Pool
	schema      string
	tablePrefix string
	commit      func(context.Context, pgx.Tx) error
	attempt     func(context.Context, string, uint64, []byte) error
	delete      func(context.Context, string) error
}

func New(pool *pgxpool.Pool, schema, tablePrefix string) *Store {
	store := &Store{pool: pool, schema: schema, tablePrefix: tablePrefix, commit: func(ctx context.Context, tx pgx.Tx) error { return tx.Commit(ctx) }}
	store.attempt = store.appendOnce
	store.delete = store.deleteOnce
	return store
}

func (s *Store) Append(ctx context.Context, name string, expected uint64, payload []byte) error {
	if err := guard.RequireDeadline(ctx, "Ledger.Append"); err != nil {
		return err
	}
	if err := storage.ValidateName(name); err != nil {
		return err
	}
	for attempt := 0; attempt < pginternal.Attempts(); attempt++ {
		err := s.attempt(ctx, name, expected, payload)
		if err == nil {
			return nil
		}
		var commitErr *commitOutcomeError
		if errors.As(err, &commitErr) {
			return s.resolveAppend(ctx, name, expected, payload, commitErr.cause)
		}
		var conflict *storage.ConflictError
		if errors.As(err, &conflict) {
			return conflict
		}
		if pginternal.Retryable(err) {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if attempt+1 < pginternal.Attempts() {
				continue
			}
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if !pginternal.Retryable(err) || attempt+1 == pginternal.Attempts() {
			return pginternal.RedactedError("ledger append")
		}
	}
	panic("unreachable")
}

func (s *Store) appendOnce(ctx context.Context, name string, expected uint64, payload []byte) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	scopes := pginternal.Qualified(s.schema, s.tablePrefix+"ledger_scopes")
	records := pginternal.Qualified(s.schema, s.tablePrefix+"ledger_records")
	if _, err = tx.Exec(ctx, "INSERT INTO "+scopes+" (name, tip) VALUES ($1, 0) ON CONFLICT (name) DO NOTHING", name); err != nil {
		return err
	}
	var tip uint64
	if err = tx.QueryRow(ctx, "SELECT tip FROM "+scopes+" WHERE name = $1 FOR UPDATE", name).Scan(&tip); err != nil {
		return err
	}
	if tip != expected {
		return &storage.ConflictError{Name: name, Expected: expected}
	}
	if _, err = tx.Exec(ctx, "INSERT INTO "+records+" (name, seq, payload) VALUES ($1, $2, $3)", name, expected+1, payload); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "UPDATE "+scopes+" SET tip = $2 WHERE name = $1", name, expected+1); err != nil {
		return err
	}
	if err := s.commit(ctx, tx); err != nil {
		if pginternal.Retryable(err) {
			return err
		}
		return &commitOutcomeError{cause: err}
	}
	return nil
}

// commitOutcomeError marks the one transaction failure point whose result may
// already be durable. Earlier failures are definite and must not be licensed
// as success by an absent authoritative read.
type commitOutcomeError struct{ cause error }

func (e *commitOutcomeError) Error() string { return "ledger commit outcome unknown" }
func (e *commitOutcomeError) Unwrap() error { return e.cause }

func (s *Store) resolveAppend(ctx context.Context, name string, expected uint64, payload []byte, cause error) error {
	checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pginternal.AuthoritativeReadTimeout())
	defer cancel()
	var stored []byte
	err := s.pool.QueryRow(checkCtx, "SELECT payload FROM "+pginternal.Qualified(s.schema, s.tablePrefix+"ledger_records")+" WHERE name = $1 AND seq = $2", name, expected+1).Scan(&stored)
	if err == nil && bytes.Equal(stored, payload) {
		return nil
	}
	if err == nil {
		return &storage.ConflictError{Name: name, Expected: expected}
	}
	return &storage.AmbiguousError{Name: name, Expected: expected, Cause: cause}
}

func (s *Store) Read(ctx context.Context, name string, from uint64) (storage.Cursor, error) {
	if err := guard.RequireDeadline(ctx, "Ledger.Read"); err != nil {
		return nil, err
	}
	if err := storage.ValidateName(name); err != nil {
		return nil, err
	}
	if from < 1 {
		from = 1
	}
	rows, err := s.pool.Query(ctx, "SELECT seq, payload FROM "+pginternal.Qualified(s.schema, s.tablePrefix+"ledger_records")+" WHERE name = $1 AND seq >= $2 ORDER BY seq", name, from)
	if err != nil {
		return nil, operationFailure(ctx, "ledger read")
	}
	defer rows.Close()
	var records []storage.Record
	for rows.Next() {
		var record storage.Record
		if err := rows.Scan(&record.Seq, &record.Payload); err != nil {
			return nil, pginternal.RedactedError("ledger read")
		}
		records = append(records, record)
	}
	if rows.Err() != nil {
		return nil, operationFailure(ctx, "ledger read")
	}
	return &cursor{records: records}, nil
}

func (s *Store) Tip(ctx context.Context, name string) (uint64, error) {
	if err := guard.RequireDeadline(ctx, "Ledger.Tip"); err != nil {
		return 0, err
	}
	if err := storage.ValidateName(name); err != nil {
		return 0, err
	}
	var tip uint64
	err := s.pool.QueryRow(ctx, "SELECT tip FROM "+pginternal.Qualified(s.schema, s.tablePrefix+"ledger_scopes")+" WHERE name = $1", name).Scan(&tip)
	if err == pgx.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, operationFailure(ctx, "ledger tip")
	}
	return tip, nil
}

func (s *Store) Delete(ctx context.Context, name string) error {
	if err := guard.RequireDeadline(ctx, "Ledger.Delete"); err != nil {
		return err
	}
	if err := storage.ValidateName(name); err != nil {
		return err
	}
	if err := s.delete(ctx, name); err != nil {
		return s.resolveDelete(name, safeCause(ctx, "ledger delete"))
	}
	return nil
}

func (s *Store) deleteOnce(ctx context.Context, name string) error {
	_, err := s.pool.Exec(ctx, "DELETE FROM "+pginternal.Qualified(s.schema, s.tablePrefix+"ledger_scopes")+" WHERE name = $1", name)
	return err
}

func (s *Store) resolveDelete(name string, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), pginternal.AuthoritativeReadTimeout())
	defer cancel()
	var present int
	err := s.pool.QueryRow(ctx, "SELECT 1 FROM "+pginternal.Qualified(s.schema, s.tablePrefix+"ledger_scopes")+" WHERE name = $1", name).Scan(&present)
	if err == pgx.ErrNoRows {
		return nil
	}
	if err == nil {
		return cause
	}
	return pginternal.RedactedError("ledger delete outcome resolution")
}

func safeCause(ctx context.Context, operation string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return pginternal.RedactedError(operation)
}

type cursor struct {
	records  []storage.Record
	position int
	closed   bool
}

func (c *cursor) Next(ctx context.Context) (storage.Record, error) {
	if err := guard.RequireDeadline(ctx, "Ledger.Cursor.Next"); err != nil {
		return storage.Record{}, err
	}
	if c.closed || c.position >= len(c.records) {
		return storage.Record{}, io.EOF
	}
	record := c.records[c.position]
	record.Payload = bytes.Clone(record.Payload)
	c.position++
	return record, nil
}

func (c *cursor) Close() error { c.closed = true; return nil }

func operationFailure(ctx context.Context, operation string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return pginternal.RedactedError(operation)
}

var _ storage.Ledger = (*Store)(nil)
