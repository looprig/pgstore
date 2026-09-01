package pgstore

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pginternal "github.com/looprig/pgstore/internal/postgres"
)

const currentSchemaVersion = 1

//go:embed migrations/*.sql
var migrationFiles embed.FS

func migrate(ctx context.Context, pool *pgxpool.Pool, schema, prefix string, mode MigrationMode) error {
	if mode == MigrationDisabled {
		return nil
	}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return migrationFailure(ctx)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// The parameterized lock key serializes migration owners across processes.
	//
	// FORWARD CONSTRAINT (P1.3 step 2): the prohibition on advisory locks is
	// scoped to lease and operation code, where correctness must survive each
	// call landing on a different physical pool connection. This lock is
	// exempt and must stay: it is transaction-scoped, taken and released
	// inside one migration transaction on one connection, and it guards DDL,
	// not a lease. A blanket ban expressed as a dependency or source test must
	// carve this call site out rather than delete it.
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", schema); err != nil {
		return migrationFailure(ctx)
	}
	if _, err = tx.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+pginternal.QuoteIdentifier(schema)); err != nil {
		return migrationFailure(ctx)
	}
	versionTable := pginternal.Qualified(schema, prefix+"schema_migrations")
	if _, err = tx.Exec(ctx, "CREATE TABLE IF NOT EXISTS "+versionTable+" (version bigint PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())"); err != nil {
		return migrationFailure(ctx)
	}
	var version int
	if err = tx.QueryRow(ctx, "SELECT COALESCE(MAX(version), 0) FROM "+versionTable).Scan(&version); err != nil {
		return migrationFailure(ctx)
	}
	if version > currentSchemaVersion {
		return errors.New("pgstore: database schema is newer than this pgstore build")
	}
	if mode == MigrationValidate {
		if version != currentSchemaVersion {
			return fmt.Errorf("pgstore: database schema version %d, want %d", version, currentSchemaVersion)
		}
		return commitMigration(ctx, tx)
	}

	entries, err := fs.Glob(migrationFiles, "migrations/[0-9][0-9][0-9][0-9]_*.sql")
	if err != nil {
		return errors.New("pgstore: embedded migrations unavailable")
	}
	sort.Strings(entries)
	for _, path := range entries {
		base := strings.TrimPrefix(path, "migrations/")
		n, parseErr := strconv.Atoi(base[:4])
		if parseErr != nil || n <= version {
			continue
		}
		// COVERAGE NOTE: with exactly one embedded migration this branch cannot
		// be reached by any input — n is 1 and version is 0 whenever the loop
		// body runs — so no test kills a mutation that disables it, and none
		// pretends to. It becomes reachable, and must be covered, when P1.3 or
		// P1.4 adds the second migration file.
		if n != version+1 {
			return errors.New("pgstore: embedded migrations are not monotonic")
		}
		sqlBytes, readErr := migrationFiles.ReadFile(path)
		if readErr != nil {
			return errors.New("pgstore: embedded migration unavailable")
		}
		replacements := map[string]string{
			"{{schema}}":         pginternal.QuoteIdentifier(schema),
			"{{ledger_scopes}}":  pginternal.QuoteIdentifier(prefix + "ledger_scopes"),
			"{{ledger_records}}": pginternal.QuoteIdentifier(prefix + "ledger_records"),
			"{{kv}}":             pginternal.QuoteIdentifier(prefix + "kv"),
		}
		statement := string(sqlBytes)
		for token, identifier := range replacements {
			statement = strings.ReplaceAll(statement, token, identifier)
		}
		if _, err = tx.Exec(ctx, statement); err != nil {
			return migrationFailure(ctx)
		}
		if _, err = tx.Exec(ctx, "INSERT INTO "+versionTable+" (version) VALUES ($1)", n); err != nil {
			return migrationFailure(ctx)
		}
		version = n
	}
	if version != currentSchemaVersion {
		return errors.New("pgstore: embedded migrations did not reach current version")
	}
	return commitMigration(ctx, tx)
}

func commitMigration(ctx context.Context, tx pgx.Tx) error {
	if err := tx.Commit(ctx); err != nil {
		return migrationFailure(ctx)
	}
	return nil
}

func migrationFailure(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return pginternal.RedactedError("schema migration")
}
