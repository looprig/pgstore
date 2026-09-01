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
inside an explicit Read Committed transaction, then rechecks identity, advances the
counter, and inserts the record atomically. Duplicate identities return their stored record
without validating candidate fields or advancing the counter.

Stable keys use PostgreSQL `bytea`, not `text`. Storage accepts every valid UTF-8
value from 1 through 256 bytes, including U+0000, which PostgreSQL text cannot
represent. The provider binds and scans the original UTF-8 bytes while cursors retain
their canonical JSON string form. PostgreSQL's `bytea` B-tree comparison is
lexicographic over those bytes, preserving the released stable-key tuple order for
valid UTF-8 while covering the contract's complete value domain.

Update and Delete lock one authoritative row and implement the exact validation,
tombstone, CAS, and revision-exhaustion precedence from Storage. Rank and due moves
change in the same statement as value and revision. Serialization/deadlock errors,
if PostgreSQL raises them, are classified by SQLSTATE and retried only within the
existing bounded attempt budget; caller cancellation always stops retrying.

## Transaction isolation proof

The task requires a *transactional* per-scope counter, not PostgreSQL Serializable
isolation. Read Committed plus explicit locks is the stronger bounded-progress
choice for this protocol:

- Create's only predicate decision is whether one identity exists. Reads before the
  scope lock are optimizations only. After locking the unique
  `(namespace, ordering_scope)` counter row, Create rereads the identity, so every
  creator in that scope observes the preceding creator before deciding to advance
  the counter and insert. Counter advancement and record insertion commit or roll
  back together; the identity primary key remains the final integrity constraint.
- Update and Delete make existence, tombstone, revision, and exhaustion decisions
  only after `SELECT ... FOR UPDATE` locks the single authoritative identity row.
  The value, revision, rank, due, and tombstone fields then move in one SQL update.
- Get and each list page are single read statements. Their keyset predicates do not
  drive a later write, and the released contract does not promise a snapshot across
  separate page calls. Serializable would not create such a cross-call snapshot.
- Ambiguous-commit resolution uses a new bounded authoritative read and accepts only
  the exact full post-state. A later revision or any other intervening state remains
  ambiguous independently of the original transaction's isolation level.

The review reproduction on PostgreSQL 17 ran the released 100-writer distinct
Create case five times under Read Committed; every run completed 100/100. Changing
only Create to Serializable completed 35/100 before the four-attempt bound and
PostgreSQL logged repeated SQLSTATE `40001` (`could not serialize access due to
concurrent update`). Serializable therefore adds abort churn to a workload already
linearized by one scope row, without protecting another predicate or range.

## Queries and cursors

Direct Get uses the identity primary key. ListOrdered uses an ascending immutable
order keyset. ListRanked uses the descending
`(rank, stable_key, ordering_scope)` keyset within `(namespace, ranking_scope)`.
ListDue uses the ascending `(due_at, stable_key, ordering_scope)` keyset within a
namespace and fixed due bound. There is no OFFSET, historical scan, KV fallback, or
in-memory global sort.

The three page statements and their ordered bound arguments are constructed by one
internal package used by both production and the tagged EXPLAIN gate. The plan gate
therefore executes the exact first/middle-page SQL rather than a hand-copied facsimile.
Its unit guard parses both call sites with Go's AST: each unique `*Store` listing method
must pass its one directly built family statement's SQL and variadic arguments to its
one receiver-bound `Query`. The tagged integration test ranges directly over one helper
that owns exactly six family/page-labelled first/middle builder calls, and its sole
`QueryRow` must consume that range value on the transaction it opened. Family/page
metadata is checked against the cursor argument, including typed nil. This structural
check is insensitive to harmless formatting while copied SQL, unused/dead builders,
decoy queries, receiver shadowing, duplicate methods, and an alternate live case table
cannot satisfy statement ownership.
ListOrdered qualifies the physical numeric `order_id` in its `ORDER BY`: the selected
value is cast to text for lossless unsigned decoding, and an unqualified name would
otherwise make PostgreSQL sort by that text output expression instead of walking the
numeric B-tree order.

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
explicit-isolation/lock guards, concurrent Update/Delete CAS tests,
and tagged EXPLAIN ANALYZE assertions at representative cardinality for first and
middle order/rank/due pages. Verification includes unit and integration race tests,
PgBouncer transaction mode, mutation testing, `make check`, vulnerability scanning,
and tidy/diff cleanliness. Production SQL and bound-argument mutations must be killed
by the named EXPLAIN gate, including argument value, dynamic type, and order drift.
