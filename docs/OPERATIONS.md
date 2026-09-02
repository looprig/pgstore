# Operating pgstore

This page is the operator's contract for `pgstore`. Every claim here was
measured against disposable databases on 2026-09-01 at commit `2c1d139`, and
where something is **not** guaranteed it is stated as an absence — an omission
would read as a guarantee.

## Supported PostgreSQL versions

The supported set is the majors still inside the PostgreSQL project's five-year
support window. PostgreSQL 13 reached end of life on 2025-11-13 and is out of
scope; it is not tested and no claim is made about it.

The full integration suite — schema migrations, Storage's Ledger, Leaser, KV and
OrderedIndex conformance, and pgstore's own crash, cancellation and
ambiguous-commit tests — was run under `-race` against a disposable server of
each major:

| Major | Server version tested | Migrations | Storage conformance | Crash / ambiguous commit | Full suite |
|---|---|---|---|---|---|
| 14 | 14.24 | pass | pass | pass | **pass** (6/6 packages) |
| 15 | 15.19 | pass | pass | pass | **pass** (6/6 packages) |
| 16 | 16.15 | pass | pass | pass | **pass** (6/6 packages) |
| 17 | 17.11 | pass | pass | pass | **pass** (6/6 packages) |
| 18 | 18.6  | pass | pass | pass | **pass** (6/6 packages) |

No production SQL in this module is version-gated: the schema uses only
long-standing types and constructs, and the one non-obvious builtin,
`hashtextextended`, has existed since PostgreSQL 11.

One test, not one behaviour, was version-specific. PostgreSQL 18 began recording
`NOT NULL` constraints in `pg_constraint`, so a test asserting an exact set over
every row there saw three extra entries on 18 for a table byte-identical to the
one on 17. The assertion now excludes `contype = 'n'` and reads NOT NULL from
`pg_attribute.attnotnull`, which every supported major reports identically.

## Pool bounds

| Option | Default | Validation |
|---|---|---|
| `MaxConns` | 10 | must not be negative; `0` selects the default |
| `MinConns` | 0 | must not be negative, and must not exceed `MaxConns` |

`Schema` defaults to `looprig` and `TablePrefix` to `looprig_`. Both must be
lowercase PostgreSQL identifiers (`[a-z][a-z0-9_]*`). `Schema` is limited to 63
bytes and `TablePrefix` to 40, because PostgreSQL **silently truncates**
identifiers past 63 bytes rather than failing: 23 bytes are reserved for the
longest table suffix so that two distinct tables can never collide on one
truncated name.

Sizing is the operator's decision and this module does not adapt to load: the
pool is fixed at the configured bounds for the life of the `Store`.

## Statement and lock timeouts

| Option | Default | Validation |
|---|---|---|
| `StatementTimeout` | 30s | positive; at least 1ms |
| `LockTimeout` | 5s | positive; at least 1ms; must not exceed `StatementTimeout` |
| `LeaseTTL` | 15s | positive; at least 1ms |
| `LeaseRenewInterval` | 5s | positive; at least 1ms; must be shorter than `LeaseTTL` |

**These are transaction-scoped, not connection-scoped, and that boundary is the
important part.** `Open` deliberately removes `statement_timeout` and
`lock_timeout` from the connection's startup parameters, because a
transaction-pooling proxy does not preserve a server session between operations.
The configured limits are instead applied inside each lock-taking transaction
with `set_config('statement_timeout', …, true)`.

The consequences, stated plainly because they are easy to assume away:

- **Statements outside a transaction carry no server-side timeout.** That
  includes every `KV` operation and the single-statement read paths of the other
  primitives — `Get`, `ListOrdered`, `ListRanked`, `ListDue`, `Ledger.Read`,
  `Ledger.Tip`. On a pooled connection these inherit the server's own
  `statement_timeout`, which is `0` (unlimited) by default.
- Those paths are bounded **only** by the caller's context deadline, which pgx
  turns into query cancellation. Every operation, including `Open`, requires
  such a deadline and returns `DeadlineRequiredError` without one, so they are
  bounded — but by the caller, not by the database.
- `internal/kv` is constructed without timeout arguments at all; it opens no
  explicit transaction and issues no `set_config`.

If you need a server-enforced ceiling on those reads, set `statement_timeout` on
the database role or in the server configuration. This module will not set it
for you.

## TLS

Verified TLS is required. A DSN is accepted only if pgx resolves a TLS
configuration that does not set `InsecureSkipVerify`.

The single exception is `AllowInsecureLocalhostOnly`, which permits
`sslmode=disable` **and** only when the resolved host is a loopback address or
`localhost` **and** only when no TLS configuration is present at all. It exists
for disposable local test databases. All three conditions are required together.

`Options.DSN`, its userinfo, and the driver's own parser text are never placed
in a returned error or unwrapped from one. Do not log `Options.DSN` when
reporting an `Open` failure.

## Migration ownership

`Open` applies, validates, or bypasses the embedded schema according to
`Options.Migrations`:

- `MigrationValidate` (zero value) requires an already-current schema and never
  changes it.
- `MigrationApply` permits `Open` to take the migration lock and upgrade.
- `MigrationDisabled` bypasses the check entirely, for externally managed
  schemas.

The current schema version is **3**, recorded in `<TablePrefix>schema_migrations`.
A database whose version is **newer** than the running build is refused rather
than used.

Concurrent owners are serialized by `pg_advisory_xact_lock(hashtextextended(schema, 0))`.
That lock is **transaction-scoped**: it is taken and released inside the single
migration transaction, on one connection, and it guards DDL. It is the only
advisory lock in the module, and it is deliberately exempt from the prohibition
on advisory locks that applies to every lease and operation path — those must
stay correct when successive operations land on different physical pool
connections, which a session-scoped lock would break.

Migrations are embedded, numbered consecutively, and applied in order inside one
transaction. Upgrades from version 1 and from the immediately prior version are
covered by tests that assert no data loss.

## Transaction-pool compatibility

This module is built to run through a transaction-mode connection pooler, and
holds no server-session state to lose:

- `DefaultQueryExecMode` is `QueryExecModeExec`, so pgx does not depend on
  named prepared statements persisting between operations.
- `statement_timeout` and `lock_timeout` are removed from startup
  `RuntimeParams`; limits are applied per transaction instead.
- There is no `LISTEN`/`NOTIFY`, no temporary table, no server-side cursor, no
  `SET SESSION`, no session-scoped advisory lock, and no retained
  `pool.Acquire` connection anywhere in production code.
- Lease ownership is proved by an epoch and a random holder token compared
  inside row transactions, never by a session-held lock.

**Measured**, against `postgres:17` behind `edoburu/pgbouncer:v1.25.2-p0` in
`pool_mode = transaction`:

| Pooler setting | Result |
|---|---|
| default (`SHOW CONFIG` reports `max_prepared_statements = 200`) | the **entire** integration suite passes, all six packages |
| `MAX_PREPARED_STATEMENTS=0` | `internal/kv` and `internal/postgres` pass; `internal/ledger`, `internal/lease` and `internal/orderedindex` fail; the root package fails only `TestMigrationUpgradesImmediatelyPriorVersionWithoutDataLoss`, while the Ledger/KV/Leaser/OrderedIndex conformance tests pass |

**Read that second row carefully, because it is a fact about the test harnesses
and not about this module.** Those suites build their own pools with raw
`pgxpool.New`, which bypasses the `Open` configuration above and inherits pgx's
default statement-caching exec mode. They pass at the pooler's default because
PgBouncer has tracked and replayed named prepared statements in transaction mode
since 1.21 — that is, because the pooler compensates, not because the harnesses
exercise what `Open` configures. The conformance tests, which obtain their store
from `Open`, pass in both configurations. This is recorded as an open item in
`docs/FOLLOWUPS.md`.

Through the module's own API the error is redacted, so a failure reads
`pgstore: ledger append failed` and says nothing about prepared statements. The
raw text surfaces only from a harness pool, in two forms:
`prepared statement "stmtcache_..." does not exist` (SQLSTATE 26000) and
`... already exists` (SQLSTATE 42P05).

## Backup and restore

All state is ordinary table data in one schema, so a standard `pg_dump` of that
schema is a complete backup and needs no special handling.

**Restoring an older snapshot is not a neutral operation, and the reason is
worth understanding before you need it.** Two identifiers that callers rely on
being monotonic are derived from stored rows, not from a sequence or a clock:
the lease `epoch` is read `FOR UPDATE` and incremented, and the OrderedIndex
`next_order` counter is read and incremented in the same transaction. Restoring
rewinds both.

Measured end to end on `postgres:17` — take a dump while a lease is held and
three records exist, do more work, then drop and restore that dump:

| Stage | Observed |
|---|---|
| before backup | lease epoch 1; orders 1, 2, 3 |
| after further work | lease epoch 2; orders 4, 5, 6 |
| immediately after restore | `Acquire` fails: `storage: lease "demo" held by epoch 1` — the restored row grants ownership to a holder that no longer exists |
| after the restored expiry passes | `Acquire` returns **epoch 2**, already issued to a different holder before the backup; `Create` issues **order 4**, duplicating an order already handed out |

So a restore has two distinct effects. First, a **ghost holder**: until the
restored `expires_at` passes, the lease is held by a process that is not
running, and no one can acquire it. Second, and more serious, **identifier
reuse**: epochs and acceptance orders that were already observed by callers are
handed out a second time, which defeats the fencing that the epoch exists to
provide.

If you restore into a system whose consumers have already seen the newer
identifiers, treat the fencing guarantee as broken until you have reconciled
them. This module cannot detect the situation for you.

## Metrics and observability

**There are none, and none are planned by this module.** `pgstore` exposes no
metrics, no tracing, no callbacks and no logging of any kind. The entire
exported surface is `Open`, `Store`, `Options`, `OptionsError`, `MigrationMode`,
and the `DeadlineRequiredError` / `NotImplementedError` aliases.

This is deliberate rather than unfinished: the module never logs, because the
one thing it must never emit is a DSN or a credential, and the simplest way to
guarantee that is to have no logging path at all. Callers that need visibility
should instrument at their own call sites, or read `pgxpool`'s statistics from a
pool they construct themselves.

## Known limits

Recorded here rather than only in test comments, because a reader of the release
should see them:

- **The in-memory sort guard has a reach limit.** The source guard that forbids
  re-sorting a page in the process catches named sorting entry points, including
  `sort.Slice`, `slices.SortFunc` and a helper named `sortRecords`. It does not
  catch an unnamed inline comparison loop, and neither the database-free index
  coverage guard nor the integration plan gate can — both read only SQL. Such a
  reorder is caught by Storage's conformance ordering suite, and only with a
  database.
- **One defensive branch is unreachable and uncovered.** The monotonicity check
  in `migrations.go` cannot be reached by any input while the embedded
  migrations are consecutively numbered. It is kept as a guard against a future
  numbering mistake, and is annotated as such in the source and indexed in
  `docs/FOLLOWUPS.md`.
- **4 of the 154 mutation-campaign expectations are pinned in the fast suite.**
  `scripts/mutation-test.sh` asserts an expected failure message per mutation.
  Four of those expectations — the three that have drifted in practice, plus the
  isolation-level one — are additionally bound to the guards that produce them by
  a database-free test, so rewording a message fails in milliseconds. The other
  150 are discoverable only by a full campaign run. Cheap coverage for them does
  not exist: they are behavioural strings that need a real mutated tree and often
  a database, so reproducing them without one would mean a second implementation
  of each mutation, which is exactly the hand-maintained copy that drifts.

## Running the tests

```sh
make check                                   # fmt, vet, staticcheck, gosec, govulncheck, tests, build
GOWORK=off go test -race ./...               # no database required
PGSTORE_TEST_DSN=... GOWORK=off go test -tags integration -race ./...
PGSTORE_TEST_DSN=... sh scripts/mutation-test.sh
```

Integration tests skip when `PGSTORE_TEST_DSN` is unset. Each provisions its own
uniquely prefixed schema objects rather than sharing a fixture, so they are safe
to run concurrently against one disposable database.
