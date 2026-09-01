//go:build integration

package pgstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	dsn, store := nonceCredentialStore(t)
	// Closing the pool makes every statement fail at the driver, which is the
	// one fault that reaches all of the redacted operation and resolution paths.
	store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	operations := map[string]func() error{
		"Leaser.Acquire": func() error {
			_, err := store.Leaser.Acquire(ctx, "sessions/closed")
			return err
		},
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
		"OrderedIndex.Get": func() error {
			_, err := store.OrderedIndex.Get(ctx, storage.OrderedID{Namespace: "sessions", OrderingScope: "closed", StableKey: "key"})
			return err
		},
		"OrderedIndex.Create": func() error {
			_, _, err := store.OrderedIndex.Create(ctx, storage.OrderedID{Namespace: "sessions", OrderingScope: "closed", StableKey: "key"}, "workers", []byte("x"), storage.Rank{}, storage.Due{State: storage.NotDue})
			return err
		},
		"OrderedIndex.Update": func() error {
			_, err := store.OrderedIndex.Update(ctx, storage.OrderedID{Namespace: "sessions", OrderingScope: "closed", StableKey: "key"}, 1, []byte("x"), storage.Rank{}, storage.Due{State: storage.NotDue})
			return err
		},
		"OrderedIndex.Delete": func() error {
			_, err := store.OrderedIndex.Delete(ctx, storage.OrderedID{Namespace: "sessions", OrderingScope: "closed", StableKey: "key"}, 1)
			return err
		},
		"OrderedIndex.ListOrdered": func() error {
			_, err := store.OrderedIndex.ListOrdered(ctx, "sessions", "closed", 0, 1)
			return err
		},
		"OrderedIndex.ListRanked": func() error {
			_, err := store.OrderedIndex.ListRanked(ctx, "sessions", "workers", "", 1)
			return err
		},
		"OrderedIndex.ListDue": func() error {
			_, err := store.OrderedIndex.ListDue(ctx, "sessions", 0, "", 1)
			return err
		},
	}
	for _, name := range slices.Sorted(maps.Keys(operations)) {
		err := operations[name]()
		if err == nil {
			t.Errorf("%s on a closed pool = nil, want an error", name)
			continue
		}
		assertRedacted(t, name, dsn, err)
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
		dsn := unreachableDSN(t)
		store, err := Open(ctx, Options{
			DSN:                        dsn,
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
		assertRedacted(t, "Open/"+name, dsn, err)
	}
}

// unreachableDSN points at a loopback port with no listener, carrying nonce
// credentials. Nothing ever authenticates with them, so they need not exist;
// they exist so that every component of the DSN, the bare password included,
// is a value whose appearance in an error can only be a leak.
func unreachableDSN(t *testing.T) string {
	t.Helper()
	parsed, err := url.Parse(os.Getenv("PGSTORE_TEST_DSN"))
	if err != nil {
		t.Fatalf("parse PGSTORE_TEST_DSN: %v", err)
	}
	parsed.User = url.UserPassword("user"+randomNonce(t), randomNonce(t))
	parsed.Host = net.JoinHostPort(parsed.Hostname(), "1")
	return parsed.String()
}

// nonceCredentialStore opens a Store whose password is a fresh random value,
// so the password can be scanned for on its own. With the configured
// PGSTORE_TEST_DSN that is not possible: its password is a word that legitimately
// occurs inside redacted messages, which would force the bare-password check to
// be dropped. Owning the credential removes that compromise instead of
// justifying it.
func nonceCredentialStore(t *testing.T) (string, *Store) {
	t.Helper()
	admin := adminPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	nonce := randomNonce(t)
	role := "pgstore_redaction_" + nonce
	schema := "redaction_" + nonce
	if _, err := admin.Exec(ctx, "CREATE ROLE "+role+" LOGIN PASSWORD "+quoteLiteral(nonce)); err != nil {
		t.Fatalf("create nonce role: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cleanupCancel()
		if _, err := admin.Exec(cleanupCtx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE; DROP OWNED BY "+role+"; DROP ROLE "+role); err != nil {
			t.Errorf("drop nonce role: %v", err)
		}
	})
	var database string
	if err := admin.QueryRow(ctx, "SELECT current_database()").Scan(&database); err != nil {
		t.Fatalf("read database name: %v", err)
	}
	if _, err := admin.Exec(ctx, "GRANT CREATE, CONNECT ON DATABASE "+database+" TO "+role); err != nil {
		t.Fatalf("grant to nonce role: %v", err)
	}

	parsed, err := url.Parse(os.Getenv("PGSTORE_TEST_DSN"))
	if err != nil {
		t.Fatalf("parse PGSTORE_TEST_DSN: %v", err)
	}
	parsed.User = url.UserPassword(role, nonce)
	dsn := parsed.String()
	store, err := Open(ctx, Options{
		DSN:                        dsn,
		Schema:                     schema,
		TablePrefix:                "redaction_",
		Migrations:                 MigrationApply,
		AllowInsecureLocalhostOnly: true,
	})
	if err != nil {
		t.Fatalf("Open with nonce credentials: %v", err)
	}
	t.Cleanup(store.Close)
	return dsn, store
}

func randomNonce(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("read random nonce: %v", err)
	}
	return hex.EncodeToString(raw)
}

// quoteLiteral renders a nonce as a SQL string literal. CREATE ROLE ... PASSWORD
// does not accept a parameter, and the value is hex from crypto/rand.
func quoteLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// assertRedacted fails when an error discloses the DSN in use or any part of it
// that identifies the deployment or authenticates to it, the bare password
// included.
func assertRedacted(t *testing.T, operation, dsn string, err error) {
	t.Helper()
	secrets := []string{dsn}
	if parsed, parseErr := url.Parse(dsn); parseErr == nil {
		if password, ok := parsed.User.Password(); ok && password != "" {
			secrets = append(secrets, password, ":"+password+"@", "password="+password)
		}
		if user := parsed.User.Username(); user != "" {
			secrets = append(secrets, user, user+":")
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
