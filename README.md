# pgstore

`pgstore` is Looprig's PostgreSQL provider for four structured Storage
primitives: **Ledger**, **Leaser**, **KV**, and **OrderedIndex**. It intentionally
does not implement `storage.Blobs`; a cloud composition combines these fields
with the S3-compatible blob provider from `s3store`.

P1.2 implements migration-safe Ledger and KV over PostgreSQL. `Open` applies,
validates, or bypasses schema version `0001` according to `Options.Migrations`;
apply mode serializes owners under an explicit transaction-scoped migration
lock. Leaser and OrderedIndex remain typed `NotImplementedError` seams for
P1.3 and P1.4.

An operation whose acknowledgement is lost is resolved by an authoritative
reread, never by a cached value. For `Ledger.Append` that reread reports success
only for this caller's own payload at the expected sequence, a conflict for
another writer's record there, and `storage.AmbiguousError` when the record is
absent — absence cannot prove the append never committed, because a concurrent
`Delete` cascades records away. `KV.Put` resolves an absent key to the original
failure instead; `internal/kv/kv.go` records why that difference is deliberate.

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
skips when it is absent; each test provisions its own uniquely prefixed schema
objects rather than sharing a fixture. `scripts/mutation-test.sh` runs the
mutation campaign and needs the same DSN. Key ordering coverage depends on the
database collation: `TestKVKeysUsesMemstoreBytewiseOrdering` skips on a
bytewise-collating database, where `TestKVKeysPinsBytewiseCollation` is the
guard that still holds. Ledger/KV conformance, transaction races, migration
ownership, cancellation, and ambiguous-commit tests require a disposable
PostgreSQL database.
