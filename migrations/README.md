# PostgreSQL migrations

P1.2 introduces the first numbered schema migration together with Ledger and
KV. P1.1 deliberately does not publish an empty `0001` migration: consuming a
schema version for a no-op would make the real first schema an upgrade from a
state that never existed.
