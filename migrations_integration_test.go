//go:build integration

package pgstore

import (
	"context"
	"os"
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
