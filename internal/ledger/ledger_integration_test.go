//go:build integration

package ledger

import (
	"context"
	"errors"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	pginternal "github.com/looprig/pgstore/internal/postgres"
	"github.com/looprig/storage"
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

// TestAppendReportsAmbiguousWhenTheRecordIsAbsentAfterAnUnknownCommit covers the
// half of the authoritative reread that must NOT report success. An absent
// record cannot prove the append never committed, because a concurrent Delete
// cascades the record away; reporting nil there would tell a caller its write
// is durable when it may have been committed and then removed.
func TestAppendReportsAmbiguousWhenTheRecordIsAbsentAfterAnUnknownCommit(t *testing.T) {
	pool := prepareLedgerTestPool(t, "ambiguous_")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store := New(pool, "looprig", "ambiguous_")
	store.commit = func(commitCtx context.Context, tx pgx.Tx) error {
		// The transaction did not commit, but the caller cannot learn that: the
		// acknowledgement is what was lost.
		_ = tx.Rollback(commitCtx)
		return errors.New("simulated unknown commit outcome")
	}

	err := store.Append(ctx, "sessions/ambiguous", 0, []byte("maybe"))
	var ambiguous *storage.AmbiguousError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("Append with unknown commit outcome and absent record = %T %v, want *storage.AmbiguousError", err, err)
	}
	if ambiguous.Name != "sessions/ambiguous" || ambiguous.Expected != 0 {
		t.Fatalf("AmbiguousError = %+v, want name sessions/ambiguous at expected seq 0", ambiguous)
	}
	if ambiguous.Cause == nil {
		t.Fatal("AmbiguousError.Cause = nil, want the retained commit cause")
	}

	tip, tipErr := store.Tip(ctx, "sessions/ambiguous")
	if tipErr != nil || tip != 0 {
		t.Fatalf("Tip after ambiguous append = %d, %v; want 0, nil", tip, tipErr)
	}
}

// TestAppendReportsConflictWhenAnotherWriterOwnsTheSequence covers the other
// half: a row at the expected sequence is only this caller's success when it
// holds this caller's payload. Another writer's record at that sequence is a
// conflict, and claiming it as success is a durable wrong answer.
func TestAppendReportsConflictWhenAnotherWriterOwnsTheSequence(t *testing.T) {
	pool := prepareLedgerTestPool(t, "stolen_")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store := New(pool, "looprig", "stolen_")
	store.commit = func(commitCtx context.Context, tx pgx.Tx) error {
		_ = tx.Rollback(commitCtx)
		// A second writer wins sequence 1 while this caller's outcome is unknown.
		if _, err := pool.Exec(ctx, `INSERT INTO looprig.stolen_ledger_scopes (name, tip) VALUES ($1, 1)
ON CONFLICT (name) DO UPDATE SET tip = 1`, "sessions/stolen"); err != nil {
			return err
		}
		if _, err := pool.Exec(ctx, "INSERT INTO looprig.stolen_ledger_records (name, seq, payload) VALUES ($1, 1, $2)", "sessions/stolen", []byte("theirs")); err != nil {
			return err
		}
		return errors.New("simulated unknown commit outcome")
	}

	err := store.Append(ctx, "sessions/stolen", 0, []byte("mine"))
	var conflict *storage.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("Append when another writer owns seq 1 = %T %v, want *storage.ConflictError", err, err)
	}
	if conflict.Name != "sessions/stolen" || conflict.Expected != 0 {
		t.Fatalf("ConflictError = %+v, want name sessions/stolen at expected seq 0", conflict)
	}

	cursor, err := store.Read(ctx, "sessions/stolen", 1)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	defer func() { _ = cursor.Close() }()
	record, err := cursor.Next(ctx)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if string(record.Payload) != "theirs" {
		t.Fatalf("record at seq 1 = %q, want the other writer's payload", record.Payload)
	}
}

// TestAppendRetriesARealSerializationFailureFromPostgreSQL closes the one gap
// the injected *pgconn.PgError cannot: it proves the classifier and retry loop
// are wired to an error PostgreSQL actually raises, at the isolation level and
// on the statement the production append uses, rather than to a hand-built
// value that only resembles one. A blocked FOR UPDATE that resumes after a
// concurrent committed update raises SQLSTATE 40001 at SERIALIZABLE; the
// append must classify it, retry, and then report the honest conflict.
func TestAppendRetriesARealSerializationFailureFromPostgreSQL(t *testing.T) {
	pool := prepareLedgerTestPool(t, "real40001_")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const name = "sessions/real-serialization"
	if _, err := pool.Exec(ctx, "INSERT INTO looprig.real40001_ledger_scopes (name, tip) VALUES ($1, 0)", name); err != nil {
		t.Fatalf("seed scope: %v", err)
	}

	store := New(pool, "looprig", "real40001_")
	var mu sync.Mutex
	var attemptErrors []error
	originalAttempt := store.attempt
	store.attempt = func(attemptCtx context.Context, scope string, expected uint64, payload []byte) error {
		err := originalAttempt(attemptCtx, scope, expected, payload)
		mu.Lock()
		attemptErrors = append(attemptErrors, err)
		mu.Unlock()
		return err
	}

	blocker, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		t.Fatalf("begin blocking transaction: %v", err)
	}
	defer func() { _ = blocker.Rollback(context.WithoutCancel(ctx)) }()
	var tip uint64
	if err := blocker.QueryRow(ctx, "SELECT tip FROM looprig.real40001_ledger_scopes WHERE name = $1 FOR UPDATE", name).Scan(&tip); err != nil {
		t.Fatalf("blocking lock: %v", err)
	}

	appended := make(chan error, 1)
	go func() { appended <- store.Append(ctx, name, 0, []byte("mine")) }()
	waitForBlockedBackend(t, ctx)

	if _, err := blocker.Exec(ctx, "UPDATE looprig.real40001_ledger_scopes SET tip = 1 WHERE name = $1", name); err != nil {
		t.Fatalf("blocking update: %v", err)
	}
	if _, err := blocker.Exec(ctx, "INSERT INTO looprig.real40001_ledger_records (name, seq, payload) VALUES ($1, 1, $2)", name, []byte("theirs")); err != nil {
		t.Fatalf("blocking insert: %v", err)
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatalf("blocking commit: %v", err)
	}

	err = <-appended
	var conflict *storage.ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("Append after a real serialization failure = %T %v, want *storage.ConflictError from the retried attempt", err, err)
	}

	mu.Lock()
	observed := slices.Clone(attemptErrors)
	mu.Unlock()
	serializationFailures := 0
	for _, attemptErr := range observed {
		var pgErr *pgconn.PgError
		if errors.As(attemptErr, &pgErr) && (pgErr.Code == "40001" || pgErr.Code == "40P01") {
			serializationFailures++
			if !pginternal.Retryable(attemptErr) {
				t.Errorf("Retryable(real SQLSTATE %s) = false, want true", pgErr.Code)
			}
		}
	}
	if serializationFailures == 0 {
		t.Fatalf("no attempt observed a real serialization or deadlock SQLSTATE; attempt errors = %v", observed)
	}
	if len(observed) < 2 {
		t.Fatalf("transaction attempts = %d, want the failed attempt plus at least one retry", len(observed))
	}
}

// waitForBlockedBackend returns once PostgreSQL reports a backend waiting on a
// lock, which is the append's FOR UPDATE queued behind the test's transaction.
// Polling the server removes the race a sleep would leave.
func waitForBlockedBackend(t *testing.T, ctx context.Context) {
	t.Helper()
	observer, err := pgxpool.New(ctx, os.Getenv("PGSTORE_TEST_DSN"))
	if err != nil {
		t.Fatalf("observer pool: %v", err)
	}
	defer observer.Close()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		if err := observer.QueryRow(ctx, "SELECT count(*) FROM pg_locks WHERE NOT granted").Scan(&waiting); err != nil {
			t.Fatalf("read lock waits: %v", err)
		}
		if waiting > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no backend blocked on the scope row lock within the deadline")
}
