# Ordered Stable-Key Bytes Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Make PostgreSQL OrderedIndex accept every released valid UTF-8 StableKey, including embedded U+0000, without changing identity or pagination order.

**Architecture:** Store StableKey as `bytea`, encode every SQL identity and tuple parameter from the original UTF-8 bytes, and decode returned bytes back to `storage.StableKey` before applying the released validator. PostgreSQL `bytea` B-tree comparison is lexicographic over bytes, preserving the existing C-collated UTF-8 ordering while admitting zero bytes.

**Tech Stack:** Go 1.26, pgx/v5, PostgreSQL 17, Storage v0.6.0 StoreTest.

---

### Task 1: Reproduce the released-domain failure

**Files:**
- Modify: `internal/orderedindex/orderedindex_integration_test.go`

1. Add a public Create/Get regression with an embedded-NUL StableKey.
2. Add ordered, ranked, and due pagination records whose keys prove bytewise ordering across NUL, ASCII, and multibyte UTF-8.
3. Run the focused tagged test against disposable PostgreSQL and verify it fails because PostgreSQL `text` rejects zero bytes.

### Task 2: Preserve StableKey bytes through PostgreSQL

**Files:**
- Modify: `migrations/0003_ordered_index.sql`
- Modify: `internal/orderedindex/orderedindex.go`
- Modify: `migrations_integration_test.go`
- Modify: `orderedindex_guard_test.go`
- Modify: `orderedindex_plan_integration_test.go`

1. Change only `stable_key` to `bytea`; retain the same primary and page-index tuple positions.
2. Bind StableKey SQL parameters as cloned `[]byte` and scan `bytea` into `[]byte` before converting to the public string type.
3. Keep cursor JSON strings unchanged, but bind decoded cursor positions as bytes for ranked and due tuple predicates.
4. Update migration and EXPLAIN fixtures to insert and compare `bytea` values.
5. Run the focused public regression green, followed by migration, plan, conformance, and race tests.

### Task 3: Strengthen verification and documentation

**Files:**
- Modify: `scripts/mutation-test.sh`
- Modify: `docs/plans/2026-09-01-bounded-ordered-index-design.md`
- Modify: `README.md`

1. Add behavioral mutations that revert the migration to `text` and the SQL parameter codec to strings; require the embedded-NUL public regression to kill each.
2. Document why `bytea` is required and why its B-tree order preserves the released tuple semantics.
3. Run the complete mutation campaign and inspect every result.
4. Run `GOWORK=off make check`, tagged integration race tests, vulnerability scan, `go mod tidy -diff`, `git diff --check`, and status checks.
5. Commit repository-local files without pushing or tagging.
