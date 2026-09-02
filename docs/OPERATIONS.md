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

The "crash" column above covers the ambiguous-commit and cancellation tests, and
additionally `TestLeaseDatabaseRestartClosesLost`, which restarts the database
container underneath a held lease and asserts that `Lost()` closes. **That test
is gated on `PGSTORE_TEST_DOCKER_CONTAINER` and skips silently without it** — it
is the only test in the module that skips when `PGSTORE_TEST_DSN` is set, and an
earlier revision of this table was recorded with it skipped. Re-run with the
gate enabled it passes on all five majors:

```sh
PGSTORE_TEST_DOCKER_CONTAINER=<container name> \
PGSTORE_TEST_DSN=... GOWORK=off go test -tags integration -run TestLeaseDatabaseRestartClosesLost ./internal/lease
```

| Major | 14 | 15 | 16 | 17 | 18 |
|---|---|---|---|---|---|
| `TestLeaseDatabaseRestartClosesLost` | pass | pass | pass | pass | pass |

Set `PGSTORE_TEST_DOCKER_CONTAINER` to the name of the disposable container
serving `PGSTORE_TEST_DSN`; the test issues `docker restart` against it, so it
requires a container you own and are willing to interrupt.

**Run it on its own, with `-run`, and not as part of a full suite against a
shared container.** Restarting the database underneath a parallel run fails
every other test that happens to be mid-query: measured, setting the variable
for `go test -tags integration ./...` against one shared container turned a
clean run into dozens of unrelated failures — 38 in one measurement and 103 in
another of the same configuration, because the count depends on which tests are
mid-query when the restart lands — while the same suite without the variable and
the gated test alone both pass. That interaction is why the test is gated
rather than merely slow.

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

- **Statements outside a transaction carry no server-side timeout.** That is
  every `KV` operation, the single-statement reads `Get`, `ListOrdered`,
  `ListRanked`, `ListDue`, `Ledger.Read` and `Ledger.Tip`, **and — the one with
  the worst consequence — `Ledger.Delete`, which is a write that takes row
  locks.** On a pooled connection these inherit the server's own
  `statement_timeout`, which is `0` (unlimited) by default.
- **`Ledger.Delete` honours neither `LockTimeout` nor `StatementTimeout`.**
  Measured on `postgres:17` with `StatementTimeout=800ms`, `LockTimeout=400ms`
  and an independent session holding the scope row `FOR UPDATE`:

  | Operation | Elapsed | Outcome |
  |---|---|---|
  | `Ledger.Append` (transactional, applies `set_config`) | **414ms** | fails at `LockTimeout` |
  | `Ledger.Delete` (`s.pool.Exec`, no transaction) | **6.011s** | blocks until the caller's 6s context deadline |

  If a scope has an in-flight `Append`, a concurrent `Ledger.Delete` pins a pool
  connection for the caller's entire budget rather than failing at
  `LockTimeout`. At the default `MaxConns` of 10 that is a pool-exhaustion
  vector, so give `Ledger.Delete` a context deadline you would be willing to
  spend a connection on.
- Those paths are bounded by the caller's context deadline, which pgx turns into
  query cancellation. Every operation, including `Open`, requires such a
  deadline and returns `DeadlineRequiredError` without one — so they are
  bounded, but by the caller rather than by the database.
- **The ambiguity-resolution reads are the exception, and are bounded
  internally.** After a lost acknowledgement, `resolveAppend`, `resolveDelete`,
  `resolvePut`, `resolveDelete` (KV), `resolveAcquire`, `resolveRelease`,
  `resolveCreate` and `resolveMutation` each perform an authoritative reread on
  a context of their own with a fixed 5s budget
  (`internal/postgres.AuthoritativeReadTimeout`), deliberately detached from the
  caller's — a caller whose deadline has already expired must still learn the
  committed outcome. The lease renew loop's `observe` is bounded differently
  again: by the lease's own safety deadline, not by that constant.
- `internal/kv` is constructed without timeout arguments at all; it opens no
  explicit transaction and issues no `set_config`.

**A `statement_timeout` you put in the DSN is discarded without an error.**
Measured: for a DSN carrying `statement_timeout=200`, pgx parses
`RuntimeParams: map[statement_timeout:200]` and a raw `pgxpool` from that same
DSN reports `statement_timeout=200ms` and cancels `pg_sleep(2)` after 209ms
(SQLSTATE 57014). After `Options.resolve` the same field is `map[]`, and `Open`
accepts the DSN silently. The removal is deliberate — a startup parameter does
not survive a transaction-pooling proxy — but the obvious first attempt at
setting a ceiling is the one that is silently dropped.

If you need a server-enforced ceiling, set `statement_timeout` on the database
role (`ALTER ROLE … SET statement_timeout = …`) or in the server configuration,
where the pooler cannot lose it. This module will not set it for you, and will
not tell you that it removed yours.

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

**Measured** against `postgres:17` behind `edoburu/pgbouncer:v1.25.2-p0`. The
exact configuration matters, and neither setting below gives a deterministic
result — the default flakes occasionally, and at `max_prepared_statements = 0`
one package's outcome depends on the pooler's connection state:

```
POOL_MODE=transaction  AUTH_TYPE=scram-sha-256  ADMIN_USERS=postgres
pool_mode=transaction        max_client_conn=100      default_pool_size=20
min_pool_size=0              reserve_pool_size=0      server_lifetime=3600
server_idle_timeout=600      server_reset_query="DISCARD ALL"
server_reset_query_always=0  ignore_startup_parameters=extra_float_digits
```

Reproduce with:

```sh
PGSTORE_TEST_DSN='postgres://…@127.0.0.1:56432/postgres?sslmode=disable'   GOWORK=off go test -tags integration -race -count=1 ./...
```

| Pooler setting | Result |
|---|---|
| default (`max_prepared_statements = 200`) | all six packages pass in most runs, but **the default is not deterministic either**: `internal/ledger` failed 2 of 9 warm full-suite runs. It never failed in 8 runs of that package alone, so reproducing it needs the full parallel suite — consistent with the mechanism below |
| `MAX_PREPARED_STATEMENTS=0`, pooler just started | **not** a clean pass. `internal/ledger`, `internal/lease`, `internal/orderedindex` and the root package (`TestMigrationUpgradesImmediatelyPriorVersionWithoutDataLoss`) fail on a freshly started pooler too; only `internal/kv` and `internal/postgres` pass. Measured five ways: three `docker restart` cycles, a brand-new never-used container, and a fresh pooler run with `-p 1` |
| `MAX_PREPARED_STATEMENTS=0`, pooler with warm server connections | `internal/ledger`, `internal/lease`, `internal/orderedindex` **and `internal/kv`** fail; only `internal/postgres` passes; the root package fails only `TestMigrationUpgradesImmediatelyPriorVersionWithoutDataLoss`, and the Ledger/KV/Leaser/OrderedIndex conformance tests pass in every configuration |

**Pooler warmth changes exactly one package: `internal/kv`.** The other four
failures at `MAX_PREPARED_STATEMENTS=0` — `internal/ledger`, `internal/lease`,
`internal/orderedindex` and the root migration test — are present on a freshly
started pooler as well, so pooler freshness does not explain them. For
`internal/kv` the pooler's own connection state, not test order, decides it:
measured over three cycles alternating a `docker restart` of the pooler with a
rerun against the same one, `internal/kv` passed 3/3 on a freshly started pooler
and failed 3/3 on a reused one, and the two tests involved
(`TestResolvePutAbsentProvesCanceledCreateDidNotCommit`,
`TestPutAndDeleteResolveLostAcknowledgementsThroughPublicAPI`) behave the same
way individually. Parallel execution is enough to warm the pooler inside a
single otherwise-fresh run: fresh plus `-p 1` passes `internal/kv`, fresh plus
the default parallelism fails it. Running sequentially was tested as a rescue
hypothesis for the other four and disproved — with `-p 1` on a fresh pooler the
same four still fail. The mechanism is `server_reset_query_always=0`: in transaction
mode PgBouncer does not run `DISCARD ALL` between clients, so a server
connection keeps prepared statements created for a previous client, and a raw
pool's cached statement name may collide with, or be missing from, whichever
server connection it is next assigned.

**Read that table as a fact about the test harnesses, not about this module.**
Those suites build their own pools with raw `pgxpool.New`, which bypasses the
`Open` configuration above and inherits pgx's default statement-caching exec
mode. They mostly pass at the pooler's default because PgBouncer has tracked and
replayed named prepared statements in transaction mode since 1.21 — that is,
because the pooler compensates, not because the harnesses exercise what `Open`
configures. The conformance tests, which obtain their store from `Open`, pass in
every configuration measured. This is recorded as an open item in
`docs/FOLLOWUPS.md`.

Through the module's own API the error is redacted, so a failure reads
`pgstore: ledger append failed` and says nothing about prepared statements. The
raw text surfaces only from a harness pool, in two forms:
`prepared statement "stmtcache_..." does not exist` (SQLSTATE 26000) and
`... already exists` (SQLSTATE 42P05).

## Backup and restore

All state is ordinary table data in one schema, so a standard `pg_dump` of that
schema is a complete backup and needs no special handling.

**Restoring an older snapshot is not a neutral operation.** Three identifiers
that callers rely on being monotonic are derived from stored rows rather than
from a sequence or a clock, so restoring rewinds all three:

| Identifier | Where | How it is allocated |
|---|---|---|
| lease `epoch` | `<prefix>leases` | read `FOR UPDATE`, incremented |
| `next_order` | `<prefix>ordered_scopes` | read and incremented in the same transaction |
| `revision` | `<prefix>kv`, `<prefix>ordered_records` | the compare-and-swap token every caller passes back |

Measured end to end on `postgres:17` — take a dump, do more work, drop and
restore that dump:

| Stage | Observed |
|---|---|
| before backup | lease epoch 1; orders 1, 2; KV key at revision 3 |
| after further work | lease epoch 2; orders 3, 4; KV key at revision 4, content `"d"` |
| immediately after restore | `Acquire` fails: `storage: lease "demo" held by epoch 1` — the restored row grants ownership to a holder that no longer exists |
| after the restored expiry passes | `Acquire` returns **epoch 2**, already issued to a different holder; `Create` issues **order 4**, duplicating one already handed out |

### The `revision` rewind is the dangerous one

The lease and order rewinds fail closed or are at least observable: a ghost
holder blocks acquisition, and a duplicate order is visible in the data. **A
rewound `revision` silently destroys a write.**

Measured, continuing the same run:

1. Caller C reads the key and holds **revision 4** with content `"d"`.
2. Restore rewinds the key to **revision 3**, content `"c"`.
3. Writer B does a correct compare-and-swap at revision 3 and advances the key
   to **revision 4** with content `"B-write"`.
4. Caller C's compare-and-swap at revision 4 — a revision that now describes
   content C never saw — **succeeds**. Final content is `"C-write"`; B's write
   is gone, with no error anywhere.

That is a classic ABA lost update, and no participant can detect it.

### Order reuse violates the Storage contract

`storage.OrderedRecord` specifies that Order is *"nonzero, immutable, strictly
increasing within its order scope, and **never reused** there — including after
a tombstone."* Reissuing order 4 after a restore is therefore not merely a
surprising operational property; it is a violation of the Storage v0.6.0
contract this module implements, and downstream code is entitled to have relied
on it.

### Safe restore procedure

**Verified.** Before admitting any consumer to the restored database, push every
rewound identifier past the highest value already issued. `<margin>` is any
headroom you are comfortable with; the example below uses 1000.

```sql
-- 1 and 2: close the ghost-holder window and the order reuse.
UPDATE <schema>.<prefix>leases
   SET epoch = <highest epoch ever issued> + <margin>, holder = NULL, expires_at = NULL;
UPDATE <schema>.<prefix>ordered_scopes
   SET next_order = <highest order ever issued> + <margin>;

-- 3: close the ABA window. The two statements above do not touch any CAS token.
UPDATE <schema>.<prefix>kv              SET revision = revision + <margin>;
UPDATE <schema>.<prefix>ordered_records SET revision = revision + <margin>;
```

Measured after applying all four against the restored database above:

- `Acquire` returned in **2ms with no ghost window**, at epoch 1003 against a
  high-water mark of 2.
- `Create` issued order **1005** against a high-water mark of 4.
- Caller C's stale compare-and-swap at revision 4 **failed closed** rather than
  succeeding.
- An OrderedIndex `Update` at a stale revision 1 failed closed with
  `revision conflict: expected 1, actual 1001`.
- A caller that re-reads and then writes at the current revision still works
  normally (`Put` at revision 1005 → 1006).

Sparse allocation is explicitly permitted: Storage says order *"need not be the
next integer: allocation may be sparse"*, so the epoch and order jumps are
contract-legal. The revision bump is an out-of-band administrative change rather
than an `Update`, and its observable effect on a caller is the same as another
writer having advanced the record — a conflict, which is the fail-closed
direction.

**This procedure needs high-water marks, and that is its limit.** They are
available when you restore *over* a database you can still read, or from an
out-of-band record of the highest values issued. In a **total-loss** restore,
where the newer state is simply gone, there is no way to know how far to advance
and therefore **no safe procedure** — the only sound options are to accept that
fencing and CAS guarantees are broken until every consumer has been restarted
with fresh state, or to have kept that out-of-band record in advance. This
module cannot detect the situation for you.

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

Environment variables the test suite reads:

| Variable | Effect when unset |
|---|---|
| `PGSTORE_TEST_DSN` | every integration test skips |
| `PGSTORE_TEST_DOCKER_CONTAINER` | `TestLeaseDatabaseRestartClosesLost` skips; everything else still runs. Set it only for a targeted `-run` of that test — see above |
| `PGSTORE_MUTATION_FILTER` | the campaign runs every entry rather than one |
| `PGSTORE_MUTATION_EXPECTED_TOTAL` | the campaign pins its entry count to the script default |
| `PGSTORE_MUTATION_RECOVER_ONLY` | the campaign runs normally rather than only restoring a prior snapshot |

The full documented command set therefore leaves one test skipped unless
`PGSTORE_TEST_DOCKER_CONTAINER` is also set. Integration tests skip when
`PGSTORE_TEST_DSN` is unset. Each provisions its own
uniquely prefixed schema objects rather than sharing a fixture, so they are safe
to run concurrently against one disposable database.
