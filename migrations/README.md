# PostgreSQL migrations

`0001_ledger_kv.sql` is the first schema version. It creates only the Ledger
scope/record tables and revision-CAS KV table. P1.1 deliberately did not
publish an empty migration: consuming a version for a no-op would have made
the real first schema an upgrade from a state that never existed.

The runner records monotonic versions in a prefix-scoped migration table and
serializes owners with a transaction-scoped PostgreSQL advisory lock before it
creates the schema or version table. Later lease and ordered-index work adds
new numbered migrations; it does not rewrite `0001` after release.
