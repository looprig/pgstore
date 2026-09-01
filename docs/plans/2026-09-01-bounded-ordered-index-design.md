# Bounded PostgreSQL OrderedIndex Design

## Contract and authority

`github.com/looprig/storage` v0.6.0 is authoritative. The runbook's shorthand
`scope` means `ordering_scope` for identity/order and `ranking_scope` for ranked
pages. `ListDue` has no scope argument, so its physical index follows the released
total query order: `(namespace, due_state, due_at, stable_key, ordering_scope)`.

## Storage model

One authoritative `ordered_records` table owns identity, immutable order, revision,
value, rank, due state, and tombstones. A separate `ordered_scopes` row owns the
monotonic counter for each `(namespace, ordering_scope)`. Create locks that counter
inside a serializable transaction, rechecks identity, advances the counter, and
inserts the record atomically. Duplicate identities return their stored record
without validating candidate fields or advancing the counter.

Update and Delete lock one authoritative row and implement the exact validation,
tombstone, CAS, and revision-exhaustion precedence from Storage. Rank and due moves
change in the same statement as value and revision. Serializable/deadlock retries
are classified by SQLSTATE and never override caller cancellation.

## Queries and cursors

Direct Get uses the identity primary key. ListOrdered uses an ascending immutable
order keyset. ListRanked uses the descending
`(rank, stable_key, ordering_scope)` keyset within `(namespace, ranking_scope)`.
ListDue uses the ascending `(due_at, stable_key, ordering_scope)` keyset within a
namespace and fixed due bound. There is no OFFSET, historical scan, KV fallback, or
in-memory global sort.

Ranked and due cursors are bounded base64url-encoded versioned JSON envelopes. They
bind kind, namespace, ranking scope or due bound, and the complete last tuple.
Decoding enforces encoded and decoded ceilings, exact field shape, canonical JSON,
known version/kind, and query equality. Cursor opacity protects pagination integrity;
it is not an authorization mechanism, so live request fields remain authoritative.

## Failure and ambiguity handling

Driver errors are always redacted. Definite pre-commit failures return context or a
redacted operation error. Only a commit acknowledgement failure is ambiguous. A
fresh bounded authoritative read resolves success only when the exact committed
post-state and expected revision are visible; a later intervening mutation remains
ambiguous. Create retries converge through identity idempotency.

## Verification

The released StoreTest is the semantic baseline. Provider tests add migration and
constraint coverage, concurrent allocation, atomic rank/due movement, cancellation,
commit ambiguity, namespace isolation, source-level no-OFFSET/no-fallback guards,
and tagged EXPLAIN ANALYZE assertions at representative cardinality for first and
middle order/rank/due pages. Verification includes unit and integration race tests,
PgBouncer transaction mode, mutation testing, `make check`, vulnerability scanning,
and tidy/diff cleanliness.
