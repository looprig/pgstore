# pgstore

`pgstore` is Looprig's PostgreSQL provider for four structured Storage
primitives: **Ledger**, **Leaser**, **KV**, and **OrderedIndex**. It intentionally
does not implement `storage.Blobs`; a cloud composition combines these fields
with the S3-compatible blob provider from `s3store`.

This P1.1 scaffold validates configuration, creates a lazy pgx pool, wires the
four interfaces, and returns typed `NotImplementedError` values from operations.
The first schema, migrations, disposable-database fixture, and Ledger/KV
implementation arrive in P1.2. `Open` therefore does not ping or mutate a
database yet.

## Configuration

`Options` requires a PostgreSQL DSN using verified TLS. DSN values and parser
errors are never retained in returned errors. Plaintext is accepted only for an
explicitly enabled loopback test database. Pool minimum/maximum connections,
schema, table prefix, statement timeout, lock timeout, and migration policy are
validated before the pool is created. Every call requires a caller-owned
context deadline.

```go
ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
defer cancel()

store, err := pgstore.Open(ctx, pgstore.Options{
    DSN:        os.Getenv("DATABASE_URL"),
    Migrations: pgstore.MigrationValidate,
})
if err != nil {
    return err
}
defer store.Close()

structured := struct {
    Ledger       storage.Ledger
    Leaser       storage.Leaser
    KV           storage.KV
    OrderedIndex storage.OrderedIndex
}{store.Ledger, store.Leaser, store.KV, store.OrderedIndex}
_ = structured
```

Do not log `Options.DSN` when reporting an `Open` failure.

## Development

```sh
make check
GOWORK=off go test ./...
```

The integration-tagged Storage conformance wiring reads `PGSTORE_TEST_DSN` and
skips when it is absent. P1.2 owns making that suite operational against a
disposable PostgreSQL database.
