# Bounded OrderedIndex Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:test-driven-development to implement this plan task-by-task.

**Goal:** Implement Storage v0.6.0's bounded OrderedIndex over PostgreSQL with transactional order allocation and index-backed keyset pages.

**Architecture:** Keep records in one authoritative table and allocate immutable order from a row-locked per-order-scope counter. Serve current views from purpose-built B-tree indexes and bind provider-owned cursors to the complete query and tuple.

**Tech Stack:** Go 1.26, pgx/v5, PostgreSQL 17, Storage v0.6.0 StoreTest.

---

### Task 1: Migration and released conformance RED

**Files:**
- Create: `migrations/0003_ordered_index.sql`
- Modify: `migrations.go`
- Modify: `migrations_integration_test.go`
- Modify: `conformance_integration_test.go`

1. Add named migration and released StoreTest assertions before production changes.
2. Run the focused tagged tests and confirm failures are caused by schema/version and
   `NotImplemented`, not setup.
3. Add the counter/record tables, constraints, exact indexes, migration tokens, and
   StoreTest cursor probe.
4. Run the migration tests green; conformance remains behaviorally red.

### Task 2: Identity, create, update, and tombstone behavior

**Files:**
- Modify: `internal/orderedindex/orderedindex.go`
- Create: `internal/orderedindex/orderedindex_test.go`
- Create: `internal/orderedindex/orderedindex_integration_test.go`
- Modify: `pgstore.go`

1. Add exact named RED tests for validation precedence, duplicate idempotency,
   concurrent same/distinct create, immutable order, atomic rank/due update, CAS,
   tombstone retry/nonreuse, cancellation, and namespace isolation.
2. Implement validation and record scanning helpers.
3. Implement explicit Read Committed transactions with a row-locked per-scope
   counter and authoritative post-lock identity recheck. Keep classified
   serialization/deadlock retries bounded for errors PostgreSQL actually returns.
4. Implement exact-row Update/Delete CAS and revision exhaustion.
5. Run each focused RED green, then the mutation-side conformance subset.

### Task 3: Keyset pages and bounded cursors

**Files:**
- Modify: `internal/orderedindex/orderedindex.go`
- Modify: `internal/orderedindex/orderedindex_test.go`
- Modify: `internal/orderedindex/orderedindex_integration_test.go`

1. Add RED tests for ordered/ranked/due first and middle pages, signed extrema,
   stable-key/order-scope ties, moves, tombstones, query mismatch, wrong kind,
   malformed/unknown/oversize/noncanonical cursor, and cancellation.
2. Implement strict bounded cursor encoding/decoding.
3. Implement the three SQL keyset queries with `limit+1` exhaustion detection.
4. Run provider tests and the entire released StoreTest green.

### Task 4: Ambiguity, plans, source guards, and redaction

**Files:**
- Modify: `internal/orderedindex/orderedindex.go`
- Modify: `internal/orderedindex/orderedindex_test.go`
- Modify: `internal/orderedindex/orderedindex_integration_test.go`
- Modify: `sql_guard_test.go`
- Modify: `transaction_integration_test.go`

1. Add RED commit-fault tests for exact Create/Update/Delete outcomes and later
   intervening revisions.
2. Resolve only exact post-state through a fresh bounded read; otherwise return
   `OrderedAmbiguousError` with a safe cause.
3. Add source guards forbidding OFFSET, KV fallback, global sort, and operation
   advisory locks.
4. Seed representative cardinality, ANALYZE, disable sequential scans locally, and
   assert EXPLAIN ANALYZE first/middle pages select the intended exact indexes,
   including the no-invented-scope due index.
5. Extend every-operation credential/DSN redaction coverage.

### Task 5: Full verification and commit

1. Run focused unit race tests and tagged direct PostgreSQL integration race tests.
2. Run the relevant suite through transaction-mode PgBouncer.
3. Run mutation testing; inspect each behavioral result and restore all diffs.
4. Run `GOWORK=off make check`, vulnerability scan, `go mod tidy -diff`, and git
   diff/status checks.
5. Self-review against every P1.4 requirement and commit repository-local files as
   `feat(pgstore): add ordered index` without pushing or tagging.

## Isolation review correction

The original plan used “serializable” where the runbook requires “transactional.”
Post-implementation review reproduced the released 100-writer Create case under
both modes. Read Committed passed 100/100 in five consecutive runs. A minimal
Serializable substitution completed only 35/100 before the four classified retry
attempts were exhausted, with PostgreSQL reporting SQLSTATE `40001` concurrent
update failures. The final protocol therefore pins Read Committed and proves its
load-bearing scope/row locks and post-lock recheck through named concurrency tests,
source guards, and mutation kills.
