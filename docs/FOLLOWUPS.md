# pgstore follow-ups

Deferred work that is known, scoped, and deliberately not fixed in the task
that found it. Each entry names what is not true today, so a later task can
close it without rediscovering it.

## Integration harnesses bypass `Open`'s PgBouncer-compatible exec mode

Recorded 2026-09-01, found in the P1.4 spec review (finding F6). Pre-existing
since P1.1/P1.2; out of scope for P1.4.

`Options.pool` sets `poolConfig.ConnConfig.DefaultQueryExecMode =
pgx.QueryExecModeExec` (`options.go`), which is what makes the module usable
through a transaction-mode PgBouncer: pgx's default extended-protocol modes
prepare statements on a physical connection that the pooler may hand to another
client before the statement is executed.

Every integration harness that builds its own pool with `pgxpool.New` inherits
pgx's default exec mode instead, and therefore does not exercise the
configuration production uses:

- `newIntegrationStore` in `internal/orderedindex/orderedindex_integration_test.go`
- `adminPool` and the raw pools in `migrations_integration_test.go` and
  `transaction_integration_test.go`
- the harnesses in `internal/kv/kv_integration_test.go`,
  `internal/ledger/ledger_integration_test.go`, and
  `internal/lease/lease_integration_test.go`

Consequence: P1.4's "run through transaction-mode PgBouncer" is currently
satisfiable only for the root-package conformance tests, which obtain their
store from `Open`. Pointed at a PgBouncer DSN the harnesses above fail with
`prepared statement "..." does not exist`, which is a property of the harness
and not of the code under test.

Closing this means routing every harness pool through the same
`pgxpool.ParseConfig` + exec-mode configuration `Open` applies, rather than
through `pgxpool.New`, and then re-running the integration suite against
`edoburu/pgbouncer` in transaction mode.
