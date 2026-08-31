package pgstore

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/looprig/storage"
)

func TestOpenRequiresDeadline(t *testing.T) {
	t.Parallel()

	store, err := Open(context.Background(), Options{DSN: testDSN})
	if store != nil {
		store.Close()
		t.Fatal("Open returned a Store without a caller deadline")
	}
	var deadlineErr *DeadlineRequiredError
	if !errors.As(err, &deadlineErr) {
		t.Fatalf("Open error = %T %v, want *DeadlineRequiredError", err, err)
	}
}

func TestOperationRejectsNilContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	store, err := Open(ctx, Options{DSN: testDSN})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(store.Close)

	//lint:ignore SA1012 This test exercises the public nil-context rejection guard.
	err = store.Ledger.Append(nil, "sessions/nil-context", 0, nil)
	var deadlineErr *DeadlineRequiredError
	if !errors.As(err, &deadlineErr) {
		t.Fatalf("Append error = %T %v, want *DeadlineRequiredError", err, err)
	}
}

func TestOpenRejectsOptionsBeforePoolConstruction(t *testing.T) {
	original := newPool
	called := false
	newPool = func(context.Context, *pgxpool.Config) (*pgxpool.Pool, error) {
		called = true
		return nil, nil
	}
	t.Cleanup(func() { newPool = original })

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	store, err := Open(ctx, Options{DSN: "postgres://user:super-secret@%"})
	if store != nil || err == nil {
		t.Fatalf("Open = (%v, %v), want nil, error", store, err)
	}
	if called {
		t.Fatal("pool constructor called after invalid options")
	}
	if strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("Open error disclosed credential: %q", err)
	}
}

func TestOpenRedactsPoolConstructionError(t *testing.T) {
	original := newPool
	newPool = func(context.Context, *pgxpool.Config) (*pgxpool.Pool, error) {
		return nil, errors.New("driver retained super-secret")
	}
	t.Cleanup(func() { newPool = original })

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	store, err := Open(ctx, Options{DSN: testDSN})
	if store != nil {
		t.Fatal("Open returned Store with pool construction error")
	}
	if err == nil || strings.Contains(err.Error(), "super-secret") || errors.Unwrap(err) != nil {
		t.Fatalf("Open error = %T %v, want non-unwrapping redacted error", err, err)
	}
}

func TestStoreCloseIsNilSafeAndIdempotent(t *testing.T) {
	var nilStore *Store
	nilStore.Close()

	calls := 0
	store := &Store{closePool: func() { calls++ }}
	store.Close()
	store.Close()
	if calls != 1 {
		t.Fatalf("pool close calls = %d, want 1", calls)
	}
}

func TestOpenWiresStructuredPrimitivesWithoutBlobs(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	store, err := Open(ctx, Options{DSN: testDSN})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(store.Close)

	if store.Ledger == nil || store.Leaser == nil || store.KV == nil || store.OrderedIndex == nil {
		t.Fatalf("nil structured primitive: ledger=%v leaser=%v kv=%v ordered=%v",
			store.Ledger == nil, store.Leaser == nil, store.KV == nil, store.OrderedIndex == nil)
	}
	if _, present := reflect.TypeOf(store).Elem().FieldByName("Blobs"); present {
		t.Fatal("Store exposes a Blobs field; blob storage belongs to s3store")
	}
	if _, implements := any(store).(storage.Blobs); implements {
		t.Fatal("Store implements storage.Blobs; blob storage belongs to s3store")
	}
}

func TestOperationRequiresCallerDeadlineBeforeStubResult(t *testing.T) {
	t.Parallel()

	openCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	store, err := Open(openCtx, Options{DSN: testDSN})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(store.Close)

	err = store.Ledger.Append(context.Background(), "sessions/deadline", 0, nil)
	var deadlineErr *DeadlineRequiredError
	if !errors.As(err, &deadlineErr) {
		t.Fatalf("Append error = %T %v, want *DeadlineRequiredError", err, err)
	}
	if deadlineErr.Operation != "Ledger.Append" {
		t.Errorf("DeadlineRequiredError.Operation = %q, want %q", deadlineErr.Operation, "Ledger.Append")
	}
}

func TestOperationReturnsHonestNotImplementedError(t *testing.T) {
	t.Parallel()

	openCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	store, err := Open(openCtx, Options{DSN: testDSN})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(store.Close)

	err = store.Ledger.Append(openCtx, "sessions/unimplemented", 0, nil)
	var notImplemented *NotImplementedError
	if !errors.As(err, &notImplemented) {
		t.Fatalf("Append error = %T %v, want *NotImplementedError", err, err)
	}
	if notImplemented.Operation != "Ledger.Append" {
		t.Errorf("NotImplementedError.Operation = %q, want %q", notImplemented.Operation, "Ledger.Append")
	}
}
