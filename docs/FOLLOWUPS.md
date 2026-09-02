# pgstore follow-ups

Deferred work that is known, scoped, and deliberately not fixed in the task
that found it. Each entry names what is not true today, so a later task can
close it without rediscovering it.

Related, and deliberately not an entry here: the `COVERAGE NOTE` in
`migrations.go` records a defensive branch that no input can reach and no test
covers. It is the module's only such note; it is a disclosure, not deferred
work, and it is not owed a fix.

## Integration harnesses bypass `Open`'s pool configuration

Recorded 2026-09-01, from the P1.4 spec review (finding F6). Pre-existing since
P1.1/P1.2; out of scope for P1.4.

`Options.pool` sets `poolConfig.ConnConfig.DefaultQueryExecMode =
pgx.QueryExecModeExec` (`options.go`), which is what removes this module's
reliance on named prepared statements surviving between statements. Eleven
harness pools across six test files build their pool with `pgxpool.New`
instead, and so inherit pgx's default statement-caching exec mode:

- `internal/kv/kv_integration_test.go`
- `internal/ledger/ledger_integration_test.go`
- `internal/lease/lease_integration_test.go`
- `internal/orderedindex/orderedindex_integration_test.go`
- `migrations_integration_test.go`
- `transaction_integration_test.go`

**The gap is coverage, not breakage.** Measured on 2026-09-01 against
`postgres:17` behind `edoburu/pgbouncer:v1.25.2-p0` in `pool_mode=transaction`:

| PgBouncer setting | Result |
|---|---|
| default (`SHOW CONFIG` reports `max_prepared_statements = 200`) | every package passes in most runs, but **the default is not deterministic either**: `internal/ledger` failed 2 of 9 warm full-suite runs. It never failed in 8 runs of that package alone, so reproducing it needs the full parallel suite — consistent with the mechanism below |
| `MAX_PREPARED_STATEMENTS=0`, pooler just started | **not** a clean pass. `internal/ledger`, `internal/lease`, `internal/orderedindex` and the root package (`TestMigrationUpgradesImmediatelyPriorVersionWithoutDataLoss`) fail on a freshly started pooler too; only `internal/kv` and `internal/postgres` pass. Measured five ways: three `docker restart` cycles, a brand-new never-used container, and a fresh pooler run with `-p 1` |
| `MAX_PREPARED_STATEMENTS=0`, pooler with warm server connections | `internal/ledger`, `internal/lease`, `internal/orderedindex` **and `internal/kv`** fail; only `internal/postgres` passes; the root package fails only `TestMigrationUpgradesImmediatelyPriorVersionWithoutDataLoss`, while the Ledger/KV/Leaser/OrderedIndex conformance tests, which obtain their store from `Open`, pass in every configuration |

**Pooler warmth changes exactly one package: `internal/kv`.** The other four
failures at `max_prepared_statements = 0` — `internal/ledger`, `internal/lease`,
`internal/orderedindex` and the root migration test — occur on a freshly started
pooler as well; an earlier revision of this table claimed a fresh pooler passed
every package, and that was wrong. `internal/kv` alone flips: it passed 3/3 on a
freshly started pooler and failed 3/3 on a reused one, because the outcome
depends on whether a reused server connection still carries another client's
prepared statements, which `server_reset_query_always=0` leaves in place in
transaction mode. Parallel execution warms the pooler within a single otherwise
fresh run, so `internal/kv` passes fresh at `-p 1` and fails fresh at the default
parallelism; sequential execution does not rescue the other four, which still
fail at `-p 1` on a fresh pooler. `docs/OPERATIONS.md` records the full pooler
configuration and the reproduction command.

PgBouncer has tracked and replayed named prepared statements in transaction
mode since 1.21, so at the default configuration pgx's default exec mode
usually holds up and P1.4's "run through transaction-mode PgBouncer" is
satisfied for the whole suite, not only for the conformance tests — with the
caveat in row 1 that even the default flakes occasionally.

What is still missing is that these suites never exercise the configuration
`Open` applies. They pass through a pooler because the pooler compensates, not
because the code under test is configured the way production configures it, and
that dependence is largely invisible until `max_prepared_statements` is lowered
— as the last two rows above show.

Two symptoms are worth naming, because neither is the one a reader would guess.
Through the module's own API the error is redacted, so a failure reads
`pgstore: ledger append failed` and says nothing about prepared statements. The
raw text appears only where a harness pool reports its own error directly, in
two forms: `prepared statement "stmtcache_..." does not exist` (SQLSTATE 26000)
and `prepared statement "stmtcache_..." already exists` (SQLSTATE 42P05). Which
harnesses surface them, and how many times, varies between runs with pool
scheduling; the forms are stable, the sites and counts are not.

Closing this means routing every harness pool through the same
`pgxpool.ParseConfig` and exec-mode configuration `Open` applies, rather than
through `pgxpool.New`, and then re-running the suite at
`MAX_PREPARED_STATEMENTS=0`, where the difference is observable.
