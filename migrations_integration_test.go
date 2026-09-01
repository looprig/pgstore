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

func TestMigrationAppliesCurrentVersionFromEmptySchema(t *testing.T) {
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

func TestMigrationAddsLeaseTable(t *testing.T) {
	admin := adminPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := admin.Exec(ctx, "DROP SCHEMA IF EXISTS migrationlease CASCADE"); err != nil {
		t.Fatalf("drop schema: %v", err)
	}
	store, err := Open(ctx, Options{DSN: os.Getenv("PGSTORE_TEST_DSN"), Schema: "migrationlease", TablePrefix: "fresh_", Migrations: MigrationApply, AllowInsecureLocalhostOnly: true})
	if err != nil {
		t.Fatalf("Open with migration apply: %v", err)
	}
	store.Close()

	want := []string{"epoch", "expires_at", "holder", "name", "revision"}
	rows, err := admin.Query(ctx, `SELECT column_name FROM information_schema.columns WHERE table_schema = 'migrationlease' AND table_name = 'fresh_leases' ORDER BY column_name`)
	if err != nil {
		t.Fatalf("query lease columns: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			t.Fatalf("scan lease column: %v", err)
		}
		got = append(got, column)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate lease columns: %v", err)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("lease columns = %v, want %v", got, want)
	}
	wantConstraints := map[string]bool{
		"PRIMARY KEY (name)":                                true,
		"CHECK ((epoch >= 0))":                              true,
		"CHECK ((revision >= 0))":                           true,
		"CHECK (((holder IS NULL) = (expires_at IS NULL)))": true,
	}
	constraintRows, err := admin.Query(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid = 'migrationlease.fresh_leases'::regclass`)
	if err != nil {
		t.Fatalf("query lease constraints: %v", err)
	}
	defer constraintRows.Close()
	gotConstraints := make(map[string]bool)
	for constraintRows.Next() {
		var definition string
		if err := constraintRows.Scan(&definition); err != nil {
			t.Fatalf("scan lease constraint: %v", err)
		}
		gotConstraints[definition] = true
	}
	if err := constraintRows.Err(); err != nil {
		t.Fatalf("iterate lease constraints: %v", err)
	}
	if len(gotConstraints) != len(wantConstraints) {
		t.Fatalf("lease constraints = %v, want exactly %v", gotConstraints, wantConstraints)
	}
	for definition := range wantConstraints {
		if !gotConstraints[definition] {
			t.Fatalf("lease constraints = %v, missing %q", gotConstraints, definition)
		}
	}
	var versions int
	if err := admin.QueryRow(ctx, "SELECT count(*) FROM migrationlease.fresh_schema_migrations").Scan(&versions); err != nil {
		t.Fatalf("count migration versions: %v", err)
	}
	if versions != 3 {
		t.Fatalf("migration version rows = %d, want 3", versions)
	}
}

func TestMigrationAddsOrderedIndexTablesAndExactIndexes(t *testing.T) {
	admin := adminPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := admin.Exec(ctx, "DROP SCHEMA IF EXISTS migrationordered CASCADE"); err != nil {
		t.Fatalf("drop schema: %v", err)
	}
	store, err := Open(ctx, Options{DSN: os.Getenv("PGSTORE_TEST_DSN"), Schema: "migrationordered", TablePrefix: "exact_", Migrations: MigrationApply, AllowInsecureLocalhostOnly: true})
	if err != nil {
		t.Fatalf("Open with migration apply: %v", err)
	}
	store.Close()

	for _, table := range []string{"exact_ordered_scopes", "exact_ordered_records"} {
		var exists bool
		if err := admin.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", "migrationordered."+table).Scan(&exists); err != nil {
			t.Fatalf("find %s: %v", table, err)
		}
		if !exists {
			t.Errorf("migration did not create %s", table)
		}
	}
	var stableKeyType string
	if err := admin.QueryRow(ctx, `SELECT data_type FROM information_schema.columns WHERE table_schema='migrationordered' AND table_name='exact_ordered_records' AND column_name='stable_key'`).Scan(&stableKeyType); err != nil {
		t.Fatalf("read stable_key type: %v", err)
	}
	if stableKeyType != "bytea" {
		t.Fatalf("stable_key type = %q, want bytea for the full valid UTF-8 domain", stableKeyType)
	}

	wantIndexes := map[string]string{
		"exact_ordered_records_pkey": "namespace, ordering_scope, stable_key",
		"exact_ordered_order_idx":    "namespace, ordering_scope, order_id, stable_key",
		"exact_ordered_rank_idx":     "namespace, ranking_scope, rank_value, stable_key, ordering_scope",
		// ListDue has no scope parameter. This exact shape deliberately resolves
		// the runbook shorthand in favor of Storage v0.6.0's released API and
		// complete due tuple.
		"exact_ordered_due_idx": "namespace, due_state, due_at, stable_key, ordering_scope",
	}
	rows, err := admin.Query(ctx, `SELECT indexname, indexdef FROM pg_indexes WHERE schemaname = 'migrationordered' AND tablename = 'exact_ordered_records'`)
	if err != nil {
		t.Fatalf("query ordered indexes: %v", err)
	}
	defer rows.Close()
	got := make(map[string]string)
	for rows.Next() {
		var name, definition string
		if err := rows.Scan(&name, &definition); err != nil {
			t.Fatalf("scan ordered index: %v", err)
		}
		got[name] = definition
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate ordered indexes: %v", err)
	}
	for name, columns := range wantIndexes {
		definition, ok := got[name]
		if !ok || !indexDefinitionContainsColumns(definition, columns) {
			t.Errorf("index %s = %q, want columns (%s)", name, definition, columns)
		}
	}
	recordTable := `migrationordered.exact_ordered_records`
	invalidRows := []struct {
		name string
		sql  string
	}{
		{name: "negative counter", sql: `INSERT INTO migrationordered.exact_ordered_scopes VALUES ('sessions','bad-counter',-1)`},
		{name: "zero revision", sql: `INSERT INTO ` + recordTable + ` VALUES ('sessions','scope',convert_to('zero-revision','UTF8'),'workers',0,1,''::bytea,false,false,0,0,0,false)`},
		{name: "zero order", sql: `INSERT INTO ` + recordTable + ` VALUES ('sessions','scope',convert_to('zero-order','UTF8'),'workers',1,0,''::bytea,false,false,0,0,0,false)`},
		{name: "oversized value", sql: `INSERT INTO ` + recordTable + ` VALUES ('sessions','scope',convert_to('large','UTF8'),'workers',1,1,decode(repeat('00',1048577),'hex'),false,false,0,0,0,false)`},
		{name: "noncanonical due", sql: `INSERT INTO ` + recordTable + ` VALUES ('sessions','scope',convert_to('bad-due','UTF8'),'workers',1,1,''::bytea,false,false,0,0,1,false)`},
		{name: "active tombstone", sql: `INSERT INTO ` + recordTable + ` VALUES ('sessions','scope',convert_to('bad-delete','UTF8'),'workers',1,1,''::bytea,false,true,1,1,1,true)`},
	}
	for _, invalid := range invalidRows {
		if _, err := admin.Exec(ctx, invalid.sql); err == nil {
			t.Errorf("migration constraints accepted %s", invalid.name)
		}
	}
}

func indexDefinitionContainsColumns(definition, columns string) bool {
	normalized := strings.NewReplacer(`"`, "", " ASC", "", " DESC", "").Replace(definition)
	return strings.Contains(normalized, "("+columns+")")
}

func TestMigrationUpgradesVersionOneWithoutDataLoss(t *testing.T) {
	admin := adminPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := admin.Exec(ctx, `DROP SCHEMA IF EXISTS migrationupgrade CASCADE;
CREATE SCHEMA migrationupgrade;
CREATE TABLE migrationupgrade.old_schema_migrations (version bigint PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now());
INSERT INTO migrationupgrade.old_schema_migrations (version) VALUES (1);
CREATE TABLE migrationupgrade.old_ledger_scopes (name text PRIMARY KEY, tip bigint NOT NULL CHECK (tip >= 0));
CREATE TABLE migrationupgrade.old_ledger_records (name text NOT NULL REFERENCES migrationupgrade.old_ledger_scopes(name) ON DELETE CASCADE, seq bigint NOT NULL CHECK (seq > 0), payload bytea NOT NULL, PRIMARY KEY (name, seq));
CREATE TABLE migrationupgrade.old_kv (key text PRIMARY KEY, revision bigint NOT NULL CHECK (revision > 0), value bytea NOT NULL);
INSERT INTO migrationupgrade.old_kv (key, revision, value) VALUES ('sessions/kept', 7, decode('76616c7565', 'hex'));`); err != nil {
		t.Fatalf("seed version one schema: %v", err)
	}

	store, err := Open(ctx, Options{DSN: os.Getenv("PGSTORE_TEST_DSN"), Schema: "migrationupgrade", TablePrefix: "old_", Migrations: MigrationApply, AllowInsecureLocalhostOnly: true})
	if err != nil {
		t.Fatalf("Open version one schema: %v", err)
	}
	defer store.Close()
	value, revision, err := store.KV.Get(ctx, "sessions/kept")
	if err != nil || string(value) != "value" || revision != 7 {
		t.Fatalf("preserved KV = (%q, %d, %v), want (value, 7, nil)", value, revision, err)
	}
	var leaseTable bool
	if err := admin.QueryRow(ctx, `SELECT to_regclass('migrationupgrade.old_leases') IS NOT NULL`).Scan(&leaseTable); err != nil {
		t.Fatalf("find lease table: %v", err)
	}
	if !leaseTable {
		t.Fatal("version one upgrade did not create lease table")
	}
	var versions int
	if err := admin.QueryRow(ctx, "SELECT count(*) FROM migrationupgrade.old_schema_migrations").Scan(&versions); err != nil {
		t.Fatalf("count migration versions: %v", err)
	}
	if versions != 3 {
		t.Fatalf("migration version rows = %d, want 3", versions)
	}
}

func TestMigrationUpgradesImmediatelyPriorVersionWithoutDataLoss(t *testing.T) {
	admin := adminPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := admin.Exec(ctx, `DROP SCHEMA IF EXISTS migrationv2 CASCADE;
CREATE SCHEMA migrationv2;
CREATE TABLE migrationv2.old_schema_migrations (version bigint PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now());
INSERT INTO migrationv2.old_schema_migrations (version) VALUES (1), (2);
CREATE TABLE migrationv2.old_ledger_scopes (name text PRIMARY KEY, tip bigint NOT NULL CHECK (tip >= 0));
CREATE TABLE migrationv2.old_ledger_records (name text NOT NULL REFERENCES migrationv2.old_ledger_scopes(name) ON DELETE CASCADE, seq bigint NOT NULL CHECK (seq > 0), payload bytea NOT NULL, PRIMARY KEY (name, seq));
CREATE TABLE migrationv2.old_kv (key text PRIMARY KEY, revision bigint NOT NULL CHECK (revision > 0), value bytea NOT NULL);
CREATE TABLE migrationv2.old_leases (name text PRIMARY KEY, epoch bigint NOT NULL CHECK (epoch >= 0), holder bytea, expires_at timestamptz, revision bigint NOT NULL CHECK (revision >= 0), CHECK ((holder IS NULL) = (expires_at IS NULL)));
INSERT INTO migrationv2.old_kv (key, revision, value) VALUES ('sessions/kept-v2', 9, convert_to('kept', 'UTF8'));
INSERT INTO migrationv2.old_leases (name, epoch, holder, expires_at, revision) VALUES ('sessions/lease', 7, NULL, NULL, 3);`); err != nil {
		t.Fatalf("seed version two schema: %v", err)
	}
	store, err := Open(ctx, Options{DSN: os.Getenv("PGSTORE_TEST_DSN"), Schema: "migrationv2", TablePrefix: "old_", Migrations: MigrationApply, AllowInsecureLocalhostOnly: true})
	if err != nil {
		t.Fatalf("Open version two schema: %v", err)
	}
	defer store.Close()
	value, revision, err := store.KV.Get(ctx, "sessions/kept-v2")
	if err != nil || string(value) != "kept" || revision != 9 {
		t.Fatalf("preserved v2 KV = (%q, %d, %v), want (kept, 9, nil)", value, revision, err)
	}
	var epoch, leaseRevision uint64
	if err := admin.QueryRow(ctx, `SELECT epoch, revision FROM migrationv2.old_leases WHERE name='sessions/lease'`).Scan(&epoch, &leaseRevision); err != nil || epoch != 7 || leaseRevision != 3 {
		t.Fatalf("preserved v2 lease = (%d, %d, %v), want (7, 3, nil)", epoch, leaseRevision, err)
	}
	for _, table := range []string{"old_ordered_scopes", "old_ordered_records"} {
		var exists bool
		if err := admin.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", "migrationv2."+table).Scan(&exists); err != nil || !exists {
			t.Errorf("upgraded table %s exists = %v, err %v", table, exists, err)
		}
	}
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
		if versions != 3 {
			t.Fatalf("%s version rows = %d, want 3", table, versions)
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
INSERT INTO downgrade.future_schema_migrations (version) VALUES (4);`); err != nil {
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
	if versions != 3 {
		t.Fatalf("version rows after two applies = %d, want 3", versions)
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
