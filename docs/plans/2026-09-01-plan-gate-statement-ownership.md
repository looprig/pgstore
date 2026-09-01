# Plan Gate Statement Ownership Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Make the ordered-index plan guard structurally prove that its six first/middle EXPLAIN cases directly consume the three production query builders.

**Architecture:** Parse production and the plan integration test with Go's standard `go/parser`, but enforce only narrow explicit structures. Each `ListOrdered`, `ListRanked`, and `ListDue` method must have one `*Store` receiver, one direct family-builder assignment, and one receiver-bound `s.pool.Query` consuming that exact statement object. The integration test must range directly over the one `orderedPlanCases` helper and send that range value's statement to its sole `QueryRow`; the helper owns exactly six family/page-labelled direct builder calls whose metadata agrees with their true cursor semantics, including typed nil.

**Tech Stack:** Go 1.26 standard AST packages, Go tests, existing shell mutation runner.

---

### Task 1: Reproduce ownership false negatives

**Files:**
- Modify: `orderedindex_guard_test.go`

1. Add parser-valid RED fixtures for decoy/dead/second `Query`, copied live SQL plus an unused builder, receiver shadowing, a dead valid plan table plus a live copied table, and typed-nil middle metadata.
2. Add a separately labelled duplicate receiver-method fixture, which is intentionally invalid to the Go type checker but valid parser input and must still be rejected structurally.
3. Add GREEN fixtures for legal multiline calls and redundant parentheses.
4. Run the focused guard tests and confirm the current validator accepts the adversarial fixtures.

### Task 2: Implement structural ownership analysis

**Files:**
- Modify: `orderedindex_guard_test.go`

1. Bind the exact `*Store` receiver object and reject duplicate `List<Family>` declarations.
2. Require exactly one direct family-builder assignment and exactly one receiver-bound `pool.Query`; bind SQL and variadic Args to the assignment object.
3. Extract the plan cases into `orderedPlanCases` and make the integration test range directly over that helper call.
4. Require the test's sole `QueryRow` to consume the range object's statement and arguments.
5. Validate exactly six helper cases with one first and one middle call per family; bind family/page metadata to builder arguments and recognize parenthesized typed nil as first.
6. Preserve anti-vacuity and legal formatting behavior, then run the focused race tests.

### Task 3: Mutation and repository verification

**Files:**
- Modify: `scripts/mutation-test.sh`
- Modify: `docs/plans/2026-09-01-bounded-ordered-index-design.md`

1. Add compilable real-source mutations proving copied ranked/due/ordered SQL, unused/decoy queries, and alternate plan-loop sources are killed by the named ownership guard.
2. Demonstrate the previous raw-substring guard survives the whitespace-varied ranked/due copies.
3. Run focused tests, the relevant mutation delta/full campaign if the runner changes, integration race tests, `make check`, vulnerability scanning, `go mod tidy -diff`, `gofmt`, and `git diff --check`.
4. Commit repository-local files only; do not push, tag, or run PgBouncer.
