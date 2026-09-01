// Package postgres contains shared PostgreSQL mechanics for pgstore adapters.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const maxTransactionAttempts = 4

func QuoteIdentifier(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}

// Qualified returns a quoted, schema-qualified table identifier. Both inputs
// have already passed pgstore's strict lowercase-identifier validation.
func Qualified(schema, table string) string {
	return QuoteIdentifier(schema) + "." + QuoteIdentifier(table)
}

// Retryable reports only PostgreSQL serialization and deadlock SQLSTATEs.
func Retryable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "40001" || pgErr.Code == "40P01")
}

func Attempts() int { return maxTransactionAttempts }

func AuthoritativeReadTimeout() time.Duration { return 5 * time.Second }

// SetLocalTimeouts applies transaction-scoped limits without relying on a
// retained PostgreSQL session, so callers remain compatible with transaction
// pooling proxies.
func SetLocalTimeouts(ctx context.Context, tx pgx.Tx, statementTimeout, lockTimeout time.Duration) error {
	_, err := tx.Exec(ctx, "SELECT set_config('statement_timeout', $1, true), set_config('lock_timeout', $2, true)", strconv.FormatInt(statementTimeout.Milliseconds(), 10), strconv.FormatInt(lockTimeout.Milliseconds(), 10))
	return err
}

// RedactedError deliberately retains neither driver text nor connection data.
func RedactedError(operation string) error {
	return fmt.Errorf("pgstore: %s failed", operation)
}
