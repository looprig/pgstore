# Plan Gate Statement Ownership Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Make the ordered-index plan guard structurally prove that its six first/middle EXPLAIN cases directly consume the three production query builders.

**Architecture:** Parse the plan integration test with Go's standard `go/parser` and inspect the composite literals assigned to its plan-case table. Require exactly two direct calls to each `orderedquery.Ordered`, `Ranked`, and `Due` builder, with nil versus non-nil cursor positions distinguishing first and middle ranked/due pages. Keep this logic test-only and exercise it against source fixtures so copied SQL, unused builder calls, and harmless formatting are classified without brittle SQL substring matching.

**Tech Stack:** Go 1.26 standard AST packages, Go tests, existing shell mutation runner.

---

### Task 1: Reproduce ownership false negatives

**Files:**
- Modify: `orderedindex_guard_test.go`

1. Extract the statement-ownership analysis behind a helper accepting source bytes.
2. Add table-driven RED fixtures for exact and whitespace-varied copied production SQL plus unused builder calls.
3. Add GREEN fixtures for legal multiline calls and redundant parentheses.
4. Run `GOWORK=off go test -race -run TestOrderedIndexPlanGate ./...` and confirm the copied/unused fixtures fail for the current substring guard.

### Task 2: Implement structural ownership analysis

**Files:**
- Modify: `orderedindex_guard_test.go`

1. Parse source with `go/parser`.
2. Locate the plan-case composite literal structurally, without depending on local variable names.
3. Require six statement fields and direct builder calls: two calls per family.
4. Require ranked/due first calls to pass `nil` positions and middle calls to pass non-nil positions; reject aliases, SQL literals, and unrelated unused calls.
5. Preserve anti-vacuity checks and clear diagnostics.
6. Run focused unit race tests and confirm RED fixtures are rejected and legal formatting fixtures pass.

### Task 3: Mutation and repository verification

**Files:**
- Modify: `scripts/mutation-test.sh`
- Modify: `docs/plans/2026-09-01-bounded-ordered-index-design.md`

1. Add mutations proving copied ranked/due/ordered SQL plus unused builder calls are killed by the named ownership guard.
2. Demonstrate the previous raw-substring guard survives the whitespace-varied ranked/due copies.
3. Run focused tests, the relevant mutation delta/full campaign if the runner changes, integration race tests, `make check`, vulnerability scanning, `go mod tidy -diff`, `gofmt`, and `git diff --check`.
4. Commit repository-local files only; do not push, tag, or run PgBouncer.
