//go:build integration

package kv

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestResolvePutAbsentProvesCanceledCreateDidNotCommit(t *testing.T) {
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
	if _, err := pool.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS looprig; DROP TABLE IF EXISTS looprig.resolve_kv; CREATE TABLE looprig.resolve_kv (key text PRIMARY KEY, revision bigint NOT NULL, value bytea NOT NULL)`); err != nil {
		t.Fatalf("prepare table: %v", err)
	}
	store := New(pool, "looprig", "resolve_")
	cause := context.Canceled
	_, err = store.resolvePut("sessions/absent", 0, []byte("value"), cause)
	if !errors.Is(err, cause) {
		t.Fatalf("resolvePut absent error = %T %v, want original cancellation", err, err)
	}
}

func TestPutAndDeleteResolveLostAcknowledgementsThroughPublicAPI(t *testing.T) {
	store := prepareKVTestStore(t, "public_")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	originalPut := store.put
	store.put = func(callCtx context.Context, key string, expected uint64, value []byte) (uint64, error) {
		if _, err := originalPut(callCtx, key, expected, value); err != nil {
			return 0, err
		}
		return 0, errors.New("simulated lost put acknowledgement")
	}
	revision, err := store.Put(ctx, "sessions/lost-ack", 0, []byte("value"))
	if err != nil || revision != 1 {
		t.Fatalf("Put = (%d, %v), want (1, nil) after reread", revision, err)
	}
	originalDelete := store.delete
	store.delete = func(callCtx context.Context, key string) error {
		if err := originalDelete(callCtx, key); err != nil {
			return err
		}
		return errors.New("simulated lost delete acknowledgement")
	}
	if err := store.Delete(ctx, "sessions/lost-ack"); err != nil {
		t.Fatalf("Delete after lost ack: %v", err)
	}
}

func prepareKVTestStore(t *testing.T, prefix string) *Store {
	t.Helper()
	dsn := os.Getenv("PGSTORE_TEST_DSN")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS looprig; DROP TABLE IF EXISTS looprig.`+prefix+`kv; CREATE TABLE looprig.`+prefix+`kv (key text PRIMARY KEY, revision bigint NOT NULL, value bytea NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	return New(pool, "looprig", prefix)
}
