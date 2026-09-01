# CLAUDE.md — pgstore

`pgstore` implements [`storage`](../storage)'s structured primitives—Ledger,
Leaser, KV, and OrderedIndex—over PostgreSQL. It never implements Blobs; cloud
composition supplies those from `s3store`.

## Dependencies

Production code may import only:

- `github.com/looprig/storage` at its released pinned version;
- `github.com/jackc/pgx/v5` for PostgreSQL and connection pooling;
- this module's internal packages and the Go standard library.

Transitive modules belong to pgx. No local `replace`, vendor tree, ORM,
migration framework, logging framework, SessionStore, or S3 SDK is allowed.

## Database and security rules

- Use parameterized SQL for values. Schema and table identifiers come only
  from validated configuration and must be quoted.
- A table suffix appended to the validated table prefix must fit the reserve
  in `options.go`; PostgreSQL truncates identifiers past 63 bytes silently
  instead of failing.
- Never include the DSN, password, userinfo, or driver/parser error text in an
  error or log. This module does not log credentials at any level.
- Production connections require verified TLS. Plaintext is accepted only for
  an explicitly enabled loopback test database.
- Every operation, including Open, requires a caller context deadline.
- Transactions classify PostgreSQL errors by typed/code fields, never strings.
- Migrations are monotonic and serialized by an explicit database lock.
- Do not use PostgreSQL advisory locks for leases or for any primitive
  operation; P1.3 requires correctness when successive operations use
  different physical pool connections. The schema migration lock in
  `migrations.go` is the one deliberate exception: it is transaction-scoped,
  held on a single connection for the length of one DDL transaction, and is
  not a lease. P1.3's "prohibit advisory locks" requirement must be scoped to
  lease and operation code and must not remove it.
- Pool configuration must stay compatible with PgBouncer transaction pooling,
  which P1.3 and P1.5 require. Startup `RuntimeParams` and pgx's default
  statement-caching exec mode are not; see the forward constraint recorded in
  `options.go`.

## Testing and build

Unit tests require no server. PostgreSQL tests use the `integration` build tag
and read `PGSTORE_TEST_DSN` directly, skipping when it is unset; each test
provisions its own uniquely prefixed schema objects. Every Go
command uses `GOWORK=off`, and tests always run with `-race`.

Run `make check` before each commit and `make test-integration` when a disposable
PostgreSQL instance is available.

Known deferred work, including which suites are and are not runnable through a
transaction-mode PgBouncer today, is recorded in `docs/FOLLOWUPS.md`.
