# PostgreSQL migrations

`0001_ledger_kv.sql` is the first schema version. It creates only the Ledger
scope/record tables and revision-CAS KV table. P1.1 deliberately did not
publish an empty migration: consuming a version for a no-op would have made
the real first schema an upgrade from a state that never existed.

The runner records monotonic versions in a prefix-scoped migration table and
serializes owners with a transaction-scoped PostgreSQL advisory lock before it
creates the schema or version table. Later primitives add new numbered
migrations; they do not rewrite `0001` after release.

`0002_leases.sql` adds the persistent transactional epoch-lease table. A row
survives release and expiry so its greatest epoch can never reset. The nullable
holder and expiry columns are cleared together only by a matching release;
expired holders remain available for the next transactional acquisition to
replace while advancing the epoch and revision.

`0003_ordered_index.sql` adds the per-order-scope counter and authoritative
ordered-record table. Its direct, immutable-order, current-rank, and current-due
indexes match Storage v0.6.0's complete keysets. In particular, the due index is
namespace-wide because `ListDue` has no scope argument.
