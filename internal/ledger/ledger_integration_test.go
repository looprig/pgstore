//go:build integration

package ledger

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAppendResolvesCommitAcknowledgementLossByAuthoritativeRead(t *testing.T) {
	dsn := os.Getenv("PGSTORE_TEST_DSN")
	if dsn == "" {
		t.Skip("PGSTORE_TEST_DSN is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	defer pool.Close()
	const prefix = "lostack_"
	if _, err := pool.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS looprig;
DROP TABLE IF EXISTS looprig.lostack_ledger_records;
DROP TABLE IF EXISTS looprig.lostack_ledger_scopes;
CREATE TABLE looprig.lostack_ledger_scopes (name text PRIMARY KEY, tip bigint NOT NULL);
CREATE TABLE looprig.lostack_ledger_records (name text NOT NULL REFERENCES looprig.lostack_ledger_scopes(name) ON DELETE CASCADE, seq bigint NOT NULL, payload bytea NOT NULL, PRIMARY KEY(name, seq));`); err != nil {
		t.Fatalf("prepare tables: %v", err)
	}
	store := New(pool, "looprig", prefix)
	store.commit = func(commitCtx context.Context, tx pgx.Tx) error {
		if err := tx.Commit(commitCtx); err != nil {
			return err
		}
		return errors.New("simulated lost commit acknowledgement")
	}
	if err := store.Append(ctx, "sessions/lost-ack", 0, []byte("committed")); err != nil {
		t.Fatalf("Append after committed lost acknowledgement = %v, want nil after authoritative reread", err)
	}
	originalDelete := store.delete
	store.delete = func(callCtx context.Context, name string) error {
		if err := originalDelete(callCtx, name); err != nil {
			return err
		}
		return errors.New("simulated lost delete acknowledgement")
	}
	if err := store.Delete(ctx, "sessions/lost-ack"); err != nil {
		t.Fatalf("Delete after committed lost acknowledgement: %v", err)
	}
}

func TestAppendRetriesSerializationFailureAndDoesNotDuplicate(t *testing.T) {
	pool := prepareLedgerTestPool(t, "retry_")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store := New(pool, "looprig", "retry_")
	attempts := 0
	store.commit = func(commitCtx context.Context, tx pgx.Tx) error {
		attempts++
		if attempts == 1 {
			return &pgconn.PgError{Code: "40001"}
		}
		return tx.Commit(commitCtx)
	}
	if err := store.Append(ctx, "sessions/retry", 0, []byte("once")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("commit attempts = %d, want 2", attempts)
	}
	tip, err := store.Tip(ctx, "sessions/retry")
	if err != nil || tip != 1 {
		t.Fatalf("Tip = %d, %v; want 1, nil", tip, err)
	}
}

func TestAppendNeverRetriesSerializationFailureAfterCallerCancellation(t *testing.T) {
	pool := prepareLedgerTestPool(t, "cancel_")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	store := New(pool, "looprig", "cancel_")
	attempts := 0
	originalAttempt := store.attempt
	store.attempt = func(attemptCtx context.Context, name string, expected uint64, payload []byte) error {
		attempts++
		return originalAttempt(attemptCtx, name, expected, payload)
	}
	commits := 0
	store.commit = func(context.Context, pgx.Tx) error {
		commits++
		cancel()
		return &pgconn.PgError{Code: "40001"}
	}
	err := store.Append(ctx, "sessions/cancel", 0, []byte("never-committed"))
	if attempts != 1 {
		t.Fatalf("transaction attempts = %d, want 1 after caller cancellation", attempts)
	}
	if commits != 1 {
		t.Fatalf("commit attempts = %d, want 1", commits)
	}
	if err != context.Canceled {
		t.Fatalf("Append after definite serialization abort = %T %v, want context.Canceled", err, err)
	}
}

func prepareLedgerTestPool(t *testing.T, prefix string) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("PGSTORE_TEST_DSN")
	if dsn == "" {
		t.Skip("PGSTORE_TEST_DSN is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS looprig;
DROP TABLE IF EXISTS looprig.`+prefix+`ledger_records;
DROP TABLE IF EXISTS looprig.`+prefix+`ledger_scopes;
CREATE TABLE looprig.`+prefix+`ledger_scopes (name text PRIMARY KEY, tip bigint NOT NULL);
CREATE TABLE looprig.`+prefix+`ledger_records (name text NOT NULL REFERENCES looprig.`+prefix+`ledger_scopes(name) ON DELETE CASCADE, seq bigint NOT NULL, payload bytea NOT NULL, PRIMARY KEY(name, seq));`); err != nil {
		t.Fatalf("prepare tables: %v", err)
	}
	return pool
}
