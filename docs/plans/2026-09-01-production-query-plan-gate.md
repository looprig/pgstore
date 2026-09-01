# Production Query Plan Gate Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Make the ordered-index EXPLAIN integration gate execute the exact SQL and arguments used by production first and middle ordered, ranked, and due pages.

**Architecture:** Extract only the three page-statement constructors into a small `internal` package. Production listing methods and the root integration plan test both consume those constructors, so SQL text, placeholder numbering, argument types, and argument order have one authority without expanding pgstore's public API.

**Tech Stack:** Go 1.26, pgx v5, PostgreSQL 17 integration tests, shell mutation runner.

---

### Task 1: Establish the missing single-source invariant

**Files:**
- Modify: `orderedindex_guard_test.go`
- Test: `orderedindex_guard_test.go`

1. Add a guard that rejects copied ordered/ranked/due production `SELECT` statements in `orderedindex_plan_integration_test.go` and requires production to call the internal builders.
2. Run `GOWORK=off go test -race -run TestOrderedIndexPlanGateUsesProductionStatements ./...`.
3. Confirm RED because the current plan fixture embeds the six SQL statements and production has no shared builder.

### Task 2: Single-source production statements and plan inputs

**Files:**
- Create: `internal/orderedquery/orderedquery.go`
- Modify: `internal/orderedindex/orderedindex.go`
- Modify: `orderedindex_plan_integration_test.go`
- Test: `orderedindex_guard_test.go`
- Test: `orderedindex_plan_integration_test.go`

1. Add minimal internal `Ordered`, `Ranked`, and `Due` builders returning an immutable-by-convention statement value containing query text and arguments.
2. Preserve current SQL, placeholder numbering, `uint64` order formatting, byte-preserving cursor keys, limit versus limit+1 behavior, and exact argument types/order.
3. Route `ListOrdered`, `ListRanked`, and `ListDue` through the builders without changing validation, cancellation, scanning, or redacted error behavior.
4. Build all six EXPLAIN cases by calling the same builders for first and middle positions.
5. Run the focused unit guard and tagged plan integration test; confirm GREEN.

### Task 3: Mutation proof and full verification

**Files:**
- Modify: `scripts/mutation-test.sh`
- Modify: `docs/plans/2026-09-01-bounded-ordered-index-design.md`

1. Add integration mutations against production builder SQL and arguments which require `TestOrderedIndexPlansUseExactKeysetIndexesForFirstAndMiddlePages` to fail with its named index/sort assertion.
2. Run each mutation delta and record that the old copied fixture contains none of the mutation target, demonstrating it would have survived.
3. Run relevant unit/integration race tests, the full mutation campaign if the runner changed, `make check`, vulnerability scan, `go mod tidy -diff`, `gofmt`, and `git diff --check`.
4. Commit repository-local reviewed files only; do not push, tag, or run PgBouncer.
