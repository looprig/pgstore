package pgstore

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	pginternal "github.com/looprig/pgstore/internal/postgres"
	"github.com/looprig/storage"
)

// hungPostgres accepts TCP connections and never answers the startup
// message: the wedged server or pooler a deadline exists to bound. It returns
// a loopback plaintext DSN, which Options accepts only with
// AllowInsecureLocalhostOnly.
func hungPostgres(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
			go func() { _, _ = io.Copy(io.Discard, conn) }()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range conns {
			_ = conn.Close()
		}
	})
	port := listener.Addr().(*net.TCPAddr).Port
	return "postgres://looprig:secret@127.0.0.1:" + strconv.Itoa(port) + "/looprig?sslmode=disable"
}

func openHung(t *testing.T, dsn string, bound time.Duration) *Store {
	t.Helper()
	// MigrationDisabled makes Open itself perform no database I/O.
	store, err := Open(context.Background(), Options{
		DSN: dsn, AllowInsecureLocalhostOnly: true, Migrations: MigrationDisabled,
		DefaultOperationTimeout: bound,
	})
	if err != nil {
		t.Fatalf("Open without a deadline: %v", err)
	}
	t.Cleanup(store.Close)
	return store
}

func returnsWithin(t *testing.T, name string, limit time.Duration, call func() error) (time.Duration, error) {
	t.Helper()
	started := time.Now()
	done := make(chan error, 1)
	go func() { done <- call() }()
	select {
	case err := <-done:
		return time.Since(started), err
	case <-time.After(limit):
		t.Fatalf("%s did not return within %v", name, limit)
		return 0, nil
	}
}

func TestDefaultOperationTimeoutOption(t *testing.T) {
	t.Parallel()
	resolved, err := Options{DSN: testDSN}.resolve()
	if err != nil {
		t.Fatal(err)
	}
	if resolved.operationTimeout != DefaultOperationTimeout || DefaultOperationTimeout != 30*time.Second {
		t.Fatalf("zero DefaultOperationTimeout resolves to %v (default %v), want the documented 30s", resolved.operationTimeout, DefaultOperationTimeout)
	}
	for _, bad := range []time.Duration{-time.Second, time.Microsecond} {
		_, err := Options{DSN: testDSN, DefaultOperationTimeout: bad}.resolve()
		var optionsErr *OptionsError
		if !errors.As(err, &optionsErr) || optionsErr.Field != "DefaultOperationTimeout" {
			t.Errorf("DefaultOperationTimeout %v error = %T %v, want *OptionsError for the field", bad, err, err)
		}
	}
}

// TestHungDatabaseOperationsReturnWithinTheDefaultBound holds D2: with no
// caller deadline, every operation that reaches the database returns against
// a server that never answers, instead of hanging. A read returns at the
// default bound with the deadline error. A write that fails ambiguously also
// performs one authoritative reread on its own fixed budget
// (docs/OPERATIONS.md), so its ceiling is the bound plus that budget.
// Lease.Release (needs a granted lease) and Cursor.Next (no I/O) are not
// reachable against a hung server.
func TestHungDatabaseOperationsReturnWithinTheDefaultBound(t *testing.T) {
	t.Parallel()
	const bound = 300 * time.Millisecond
	store := openHung(t, hungPostgres(t), bound)
	ctx := context.Background()
	id := storage.OrderedID{Namespace: "ns", OrderingScope: "scope", StableKey: "key"}
	type operation struct {
		call  func() error
		write bool
	}
	operations := map[string]operation{
		"KV.Get":        {call: func() error { _, _, err := store.KV.Get(ctx, "kv/key"); return err }},
		"KV.Keys":       {call: func() error { _, err := store.KV.Keys(ctx, "kv/"); return err }},
		"KV.Put":        {write: true, call: func() error { _, err := store.KV.Put(ctx, "kv/key", 0, []byte("x")); return err }},
		"KV.Delete":     {write: true, call: func() error { return store.KV.Delete(ctx, "kv/key") }},
		"Ledger.Read":   {call: func() error { _, err := store.Ledger.Read(ctx, "ledger/name", 1); return err }},
		"Ledger.Tip":    {call: func() error { _, err := store.Ledger.Tip(ctx, "ledger/name"); return err }},
		"Ledger.Append": {write: true, call: func() error { return store.Ledger.Append(ctx, "ledger/name", 0, []byte("x")) }},
		"Ledger.Delete": {write: true, call: func() error { return store.Ledger.Delete(ctx, "ledger/name") }},
		"Leaser.Acquire": {write: true, call: func() error {
			_, err := store.Leaser.Acquire(ctx, "lease/name")
			return err
		}},
		"OrderedIndex.Get": {call: func() error { _, err := store.OrderedIndex.Get(ctx, id); return err }},
		"OrderedIndex.Create": {write: true, call: func() error {
			_, _, err := store.OrderedIndex.Create(ctx, id, "rank", []byte("v"), storage.Rank{}, storage.Due{})
			return err
		}},
		"OrderedIndex.Update": {write: true, call: func() error {
			_, err := store.OrderedIndex.Update(ctx, id, 1, []byte("v"), storage.Rank{}, storage.Due{})
			return err
		}},
		"OrderedIndex.Delete": {write: true, call: func() error {
			_, err := store.OrderedIndex.Delete(ctx, id, 1)
			return err
		}},
		"OrderedIndex.ListOrdered": {call: func() error {
			_, err := store.OrderedIndex.ListOrdered(ctx, "ns", "scope", 0, 10)
			return err
		}},
		"OrderedIndex.ListRanked": {call: func() error {
			_, err := store.OrderedIndex.ListRanked(ctx, "ns", "rank", storage.RankedCursor(""), 10)
			return err
		}},
		"OrderedIndex.ListDue": {call: func() error {
			_, err := store.OrderedIndex.ListDue(ctx, "ns", 0, storage.DueCursor(""), 10)
			return err
		}},
	}
	for name, op := range operations {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			limit := bound + 3*time.Second
			if op.write {
				limit += pginternal.AuthoritativeReadTimeout()
			}
			elapsed, err := returnsWithin(t, name, limit, op.call)
			if err == nil {
				t.Fatalf("%s against a hung server succeeded", name)
			}
			if elapsed < bound/2 {
				t.Fatalf("%s returned after %v, before the %v bound", name, elapsed, bound)
			}
			if !op.write && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("%s error = %T %v, want context.DeadlineExceeded", name, err, err)
			}
		})
	}
}

// TestOpenWithoutDeadlineIsBounded: Open's migration check is bounded too.
func TestOpenWithoutDeadlineIsBounded(t *testing.T) {
	t.Parallel()
	const bound = 300 * time.Millisecond
	dsn := hungPostgres(t)
	elapsed, err := returnsWithin(t, "Open", bound+3*time.Second, func() error {
		store, err := Open(context.Background(), Options{
			DSN: dsn, AllowInsecureLocalhostOnly: true, Migrations: MigrationValidate,
			DefaultOperationTimeout: bound,
		})
		if store != nil {
			store.Close()
		}
		return err
	})
	if err == nil {
		t.Fatal("Open against a hung server succeeded")
	}
	if elapsed < bound/2 {
		t.Fatalf("Open returned after %v, before the %v bound", elapsed, bound)
	}
}

// TestCallerDeadlineWinsOverTheDefault: a caller deadline is used as is,
// whether shorter or longer than the default.
func TestCallerDeadlineWinsOverTheDefault(t *testing.T) {
	t.Parallel()
	dsn := hungPostgres(t)
	t.Run("shorter", func(t *testing.T) {
		t.Parallel()
		store := openHung(t, dsn, time.Minute)
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		_, err := returnsWithin(t, "KV.Get", 3*time.Second, func() error { _, _, err := store.KV.Get(ctx, "kv/key"); return err })
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("KV.Get error = %T %v, want context.DeadlineExceeded", err, err)
		}
	})
	t.Run("longer", func(t *testing.T) {
		t.Parallel()
		store := openHung(t, dsn, 100*time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
		defer cancel()
		elapsed, _ := returnsWithin(t, "KV.Get", 5*time.Second, func() error { _, _, err := store.KV.Get(ctx, "kv/key"); return err })
		if elapsed < time.Second {
			t.Fatalf("KV.Get returned after %v; the 100ms default overrode the caller's 1.2s deadline", elapsed)
		}
	})
}

// TestCancellationWithoutDeadlineIsHonoured: the default bound is a child of
// the caller's context, so cancelling it still ends the call.
func TestCancellationWithoutDeadlineIsHonoured(t *testing.T) {
	t.Parallel()
	store := openHung(t, hungPostgres(t), time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	_, err := returnsWithin(t, "KV.Get", 3*time.Second, func() error { _, _, err := store.KV.Get(ctx, "kv/key"); return err })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("KV.Get error = %T %v, want context.Canceled", err, err)
	}
}
