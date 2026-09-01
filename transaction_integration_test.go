//go:build integration

package pgstore

import (
	"context"
	"errors"
	"maps"
	"net"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/looprig/storage"
)

func TestCanceledBeforeMutationRemainsDefiniteAndDoesNotWrite(t *testing.T) {
	store := newConformanceStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	cancel()
	if err := store.Ledger.Append(ctx, "sessions/pre-canceled", 0, []byte("x")); err != context.Canceled {
		t.Fatalf("Ledger.Append error = %T %v, want context.Canceled", err, err)
	}
	if _, err := store.KV.Put(ctx, "sessions/pre-canceled", 0, []byte("x")); err != context.Canceled {
		t.Fatalf("KV.Put error = %T %v, want context.Canceled", err, err)
	}
}

func TestConcurrentKVCASHasExactlyOneWinnerPerRevision(t *testing.T) {
	store := newConformanceStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const writers = 24
	for expected := uint64(0); expected < 2; expected++ {
		start := make(chan struct{})
		results := make(chan error, writers)
		var wg sync.WaitGroup
		for writer := range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, err := store.KV.Put(ctx, "sessions/contended", expected, []byte{byte(writer)})
				results <- err
			}()
		}
		close(start)
		wg.Wait()
		close(results)
		winners := 0
		for err := range results {
			if err == nil {
				winners++
				continue
			}
			var conflict *storage.ConflictError
			if !errors.As(err, &conflict) {
				t.Errorf("loser error = %T %v, want *storage.ConflictError", err, err)
			}
		}
		if winners != 1 {
			t.Fatalf("revision %d winners = %d, want exactly 1", expected, winners)
		}
	}
	_, revision, err := store.KV.Get(ctx, "sessions/contended")
	if err != nil || revision != 2 {
		t.Fatalf("Get revision = %d, %v; want 2, nil", revision, err)
	}
}

func TestKVKeysPrefixIsDataNotSQL(t *testing.T) {
	store := newConformanceStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := store.KV.Put(ctx, "sessions/safe", 0, []byte("value")); err != nil {
		t.Fatalf("seed Put: %v", err)
	}
	keys, err := store.KV.Keys(ctx, `sessions/'; DROP TABLE anything;--`)
	if err != nil {
		t.Fatalf("Keys with SQL-shaped prefix: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("Keys = %v, want empty", keys)
	}
	if _, _, err := store.KV.Get(ctx, "sessions/safe"); err != nil {
		t.Fatalf("Get after SQL-shaped prefix: %v", err)
	}
}

func TestKVKeysUsesMemstoreBytewiseOrdering(t *testing.T) {
	store := newConformanceStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	requireLinguisticCollation(t, ctx)
	want := []string{"a-1", "a.1", "a/1", "a0"}
	for _, key := range []string{"a0", "a/1", "a.1", "a-1"} {
		if _, err := store.KV.Put(ctx, key, 0, []byte{}); err != nil {
			t.Fatalf("Put(%q): %v", key, err)
		}
	}
	got, err := store.KV.Keys(ctx, "a")
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Keys = %v, want bytewise %v", got, want)
	}
}

// TestOperationErrorsDoNotDiscloseDSNOrCredential drives every error path the
// implemented primitives can construct, not one representative path. The
// dependency test forbids importing a logging package; nothing there stops an
// error string from interpolating driver text that retains the DSN, so each
// constructed failure must be checked at its own call site.
// requireLinguisticCollation skips rather than silently passing when the
// database orders text bytewise by default. There this assertion cannot
// distinguish a query that pins COLLATE "C" from one that does not, and the
// guarantee is held only by TestKVKeysPinsBytewiseCollation.
func requireLinguisticCollation(t *testing.T, ctx context.Context) {
	t.Helper()
	admin, err := pgxpool.New(ctx, os.Getenv("PGSTORE_TEST_DSN"))
	if err != nil {
		t.Fatalf("collation pool: %v", err)
	}
	defer admin.Close()
	var ordersBytewise bool
	if err := admin.QueryRow(ctx, "SELECT datcollate IN ('C', 'POSIX', 'C.UTF-8', 'C.utf8') FROM pg_database WHERE datname = current_database()").Scan(&ordersBytewise); err != nil {
		t.Fatalf("read database collation: %v", err)
	}
	if ordersBytewise {
		t.Skip("test database collates bytewise by default; this assertion cannot observe the explicit COLLATE \"C\", which TestKVKeysPinsBytewiseCollation guards instead")
	}
}

func TestOperationErrorsDoNotDiscloseDSNOrCredential(t *testing.T) {
	store := newConformanceStore(t)
	// Closing the pool makes every statement fail at the driver, which is the
	// one fault that reaches all of the redacted operation and resolution paths.
	store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	operations := map[string]func() error{
		"Ledger.Append": func() error { return store.Ledger.Append(ctx, "sessions/closed", 0, []byte("x")) },
		"Ledger.Read": func() error {
			_, err := store.Ledger.Read(ctx, "sessions/closed", 1)
			return err
		},
		"Ledger.Tip": func() error {
			_, err := store.Ledger.Tip(ctx, "sessions/closed")
			return err
		},
		"Ledger.Delete": func() error { return store.Ledger.Delete(ctx, "sessions/closed") },
		"KV.Get": func() error {
			_, _, err := store.KV.Get(ctx, "sessions/closed")
			return err
		},
		"KV.Put": func() error {
			_, err := store.KV.Put(ctx, "sessions/closed", 0, []byte("x"))
			return err
		},
		"KV.Keys": func() error {
			_, err := store.KV.Keys(ctx, "sessions/")
			return err
		},
		"KV.Delete": func() error { return store.KV.Delete(ctx, "sessions/closed") },
	}
	for _, name := range slices.Sorted(maps.Keys(operations)) {
		err := operations[name]()
		if err == nil {
			t.Errorf("%s on a closed pool = nil, want an error", name)
			continue
		}
		assertRedacted(t, name, err)
	}
}

// TestMigrationErrorsDoNotDiscloseDSNOrCredential covers the migration failure
// path, which is constructed before any Store exists and so cannot be reached
// through the operation table above.
func TestMigrationErrorsDoNotDiscloseDSNOrCredential(t *testing.T) {
	if os.Getenv("PGSTORE_TEST_DSN") == "" {
		t.Skip("PGSTORE_TEST_DSN is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for name, mode := range map[string]MigrationMode{"apply": MigrationApply, "validate": MigrationValidate} {
		// A loopback port with no listener fails inside migrate, after the lazy
		// pool is constructed, for every mode that performs migration work.
		store, err := Open(ctx, Options{
			DSN:                        unreachableDSN(t),
			TablePrefix:                "unreachable_",
			Migrations:                 mode,
			AllowInsecureLocalhostOnly: true,
		})
		if store != nil {
			store.Close()
			t.Fatalf("Open(%s) against an unreachable database returned a store", name)
		}
		if err == nil {
			t.Fatalf("Open(%s) against an unreachable database = nil error", name)
		}
		assertRedacted(t, "Open/"+name, err)
	}
}

// unreachableDSN keeps the test credentials of the configured DSN so that a
// leak of them is detectable, and points at a loopback port with no listener.
func unreachableDSN(t *testing.T) string {
	t.Helper()
	parsed, err := url.Parse(os.Getenv("PGSTORE_TEST_DSN"))
	if err != nil {
		t.Fatalf("parse PGSTORE_TEST_DSN: %v", err)
	}
	parsed.Host = net.JoinHostPort(parsed.Hostname(), "1")
	return parsed.String()
}

// assertRedacted fails when an error discloses the configured DSN or any part
// of it that identifies the deployment or authenticates to it.
func assertRedacted(t *testing.T, operation string, err error) {
	t.Helper()
	dsn := os.Getenv("PGSTORE_TEST_DSN")
	secrets := []string{dsn}
	if parsed, parseErr := url.Parse(dsn); parseErr == nil {
		if password, ok := parsed.User.Password(); ok && password != "" {
			// The bare password is deliberately not a secret pattern here: it is
			// a test value that can legitimately occur as a substring. Every
			// shape in which it would actually leak carries its delimiters.
			secrets = append(secrets, ":"+password+"@", "password="+password)
		}
		if user := parsed.User.Username(); user != "" {
			secrets = append(secrets, user+":")
		}
		secrets = append(secrets, parsed.Host, parsed.Hostname())
	}
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%s error disclosed %q: %v", operation, secret, err)
		}
	}
}
