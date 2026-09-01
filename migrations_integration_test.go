//go:build integration

package pgstore

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMigrationAppliesVersionOneFromEmptySchema(t *testing.T) {
	dsn := os.Getenv("PGSTORE_TEST_DSN")
	if dsn == "" {
		t.Skip("PGSTORE_TEST_DSN is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	store, err := Open(ctx, Options{DSN: dsn, Schema: "migrationtest", TablePrefix: "empty_", Migrations: MigrationApply, AllowInsecureLocalhostOnly: true})
	if err != nil {
		t.Fatalf("Open with migration apply: %v", err)
	}
	store.Close()
}

func TestConcurrentMigrationOwnersSerializeFromVersionZero(t *testing.T) {
	dsn := os.Getenv("PGSTORE_TEST_DSN")
	if dsn == "" {
		t.Skip("PGSTORE_TEST_DSN is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	defer admin.Close()
	if _, err := admin.Exec(ctx, "DROP SCHEMA IF EXISTS migrationrace CASCADE"); err != nil {
		t.Fatalf("drop schema: %v", err)
	}

	const owners = 8
	start := make(chan struct{})
	errs := make(chan error, owners)
	var wg sync.WaitGroup
	for owner := range owners {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			prefix := "first_"
			if owner%2 == 1 {
				prefix = "second_"
			}
			store, openErr := Open(ctx, Options{DSN: dsn, Schema: "migrationrace", TablePrefix: prefix, Migrations: MigrationApply, AllowInsecureLocalhostOnly: true})
			if store != nil {
				store.Close()
			}
			errs <- openErr
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent Open: %v", err)
		}
	}
	for _, table := range []string{"first_schema_migrations", "second_schema_migrations"} {
		var versions int
		if err := admin.QueryRow(ctx, "SELECT count(*) FROM migrationrace."+table).Scan(&versions); err != nil {
			t.Fatalf("read %s: %v", table, err)
		}
		if versions != 1 {
			t.Fatalf("%s version rows = %d, want 1", table, versions)
		}
	}
}

func TestMigrationValidateDoesNotCreateAbsentSchema(t *testing.T) {
	dsn := os.Getenv("PGSTORE_TEST_DSN")
	if dsn == "" {
		t.Skip("PGSTORE_TEST_DSN is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	defer admin.Close()
	if _, err := admin.Exec(ctx, "DROP SCHEMA IF EXISTS validateabsent CASCADE"); err != nil {
		t.Fatalf("drop schema: %v", err)
	}
	store, err := Open(ctx, Options{DSN: dsn, Schema: "validateabsent", TablePrefix: "check_", Migrations: MigrationValidate, AllowInsecureLocalhostOnly: true})
	if store != nil {
		store.Close()
		t.Fatal("Open validate returned store for absent schema")
	}
	if err == nil {
		t.Fatal("Open validate absent schema = nil error")
	}
	var exists bool
	if err := admin.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)", "validateabsent").Scan(&exists); err != nil {
		t.Fatalf("check schema: %v", err)
	}
	if exists {
		t.Fatal("MigrationValidate created absent schema")
	}
}

// TestMigrationRefusesASchemaNewerThanThisBuild covers the downgrade guard. A
// binary that finds a future schema must refuse both modes rather than run
// against tables whose shape it does not know.
func TestMigrationRefusesASchemaNewerThanThisBuild(t *testing.T) {
	admin := adminPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := admin.Exec(ctx, `DROP SCHEMA IF EXISTS downgrade CASCADE;
CREATE SCHEMA downgrade;
CREATE TABLE downgrade.future_schema_migrations (version bigint PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now());
INSERT INTO downgrade.future_schema_migrations (version) VALUES (2);`); err != nil {
		t.Fatalf("seed future schema: %v", err)
	}
	for name, mode := range map[string]MigrationMode{"apply": MigrationApply, "validate": MigrationValidate} {
		store, err := Open(ctx, Options{DSN: os.Getenv("PGSTORE_TEST_DSN"), Schema: "downgrade", TablePrefix: "future_", Migrations: mode, AllowInsecureLocalhostOnly: true})
		if store != nil {
			store.Close()
			t.Fatalf("Open(%s) against a future schema returned a store", name)
		}
		if err == nil || !strings.Contains(err.Error(), "newer than this pgstore build") {
			t.Fatalf("Open(%s) against a future schema = %v, want the downgrade guard", name, err)
		}
	}
	var tables int
	if err := admin.QueryRow(ctx, "SELECT count(*) FROM pg_tables WHERE schemaname = 'downgrade' AND tablename <> 'future_schema_migrations'").Scan(&tables); err != nil {
		t.Fatalf("count tables: %v", err)
	}
	if tables != 0 {
		t.Fatalf("refused migration created %d tables in the future schema, want 0", tables)
	}
}

// TestMigrationIsIdempotentAndValidatesACurrentSchema covers the two paths a
// long-lived deployment takes on every restart: a second MigrationApply that
// must not repeat version 1 or disturb stored data, and a MigrationValidate
// against an already-current schema, which must accept it.
func TestMigrationIsIdempotentAndValidatesACurrentSchema(t *testing.T) {
	admin := adminPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := admin.Exec(ctx, "DROP SCHEMA IF EXISTS reopen CASCADE"); err != nil {
		t.Fatalf("drop schema: %v", err)
	}
	options := Options{DSN: os.Getenv("PGSTORE_TEST_DSN"), Schema: "reopen", TablePrefix: "again_", Migrations: MigrationApply, AllowInsecureLocalhostOnly: true}

	first, err := Open(ctx, options)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if _, err := first.KV.Put(ctx, "sessions/kept", 0, []byte("value")); err != nil {
		t.Fatalf("seed Put: %v", err)
	}
	first.Close()

	second, err := Open(ctx, options)
	if err != nil {
		t.Fatalf("second Open with MigrationApply on a current schema: %v", err)
	}
	value, revision, err := second.KV.Get(ctx, "sessions/kept")
	if err != nil || revision != 1 || string(value) != "value" {
		t.Fatalf("Get after re-apply = (%q, %d, %v), want (\"value\", 1, nil)", value, revision, err)
	}
	second.Close()

	var versions int
	if err := admin.QueryRow(ctx, "SELECT count(*) FROM reopen.again_schema_migrations").Scan(&versions); err != nil {
		t.Fatalf("count versions: %v", err)
	}
	if versions != 1 {
		t.Fatalf("version rows after two applies = %d, want 1", versions)
	}

	options.Migrations = MigrationValidate
	validated, err := Open(ctx, options)
	if err != nil {
		t.Fatalf("Open with MigrationValidate on a current schema: %v", err)
	}
	defer validated.Close()
	if _, _, err := validated.KV.Get(ctx, "sessions/kept"); err != nil {
		t.Fatalf("Get through a validate-mode store: %v", err)
	}
}

func adminPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("PGSTORE_TEST_DSN")
	if dsn == "" {
		t.Skip("PGSTORE_TEST_DSN is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}
