# Transactional Epoch Leases Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add renewable, PostgreSQL-backed epoch leases that remain correct across pooled and transaction-pooled connections.

**Architecture:** Store one permanent row per validated lease name and serialize grants with a row lock inside a transaction. Each returned lease renews with epoch-plus-random-holder compare-and-swap operations through `pgxpool`, resolves ambiguous acknowledgements by authoritative reread, and closes one local `Lost()` channel when ownership can no longer be proved.

**Tech Stack:** Go 1.26, PostgreSQL, pgx/pgxpool v5.10.0, Storage v0.6.0 conformance, shell mutation runner.

---

### Task 1: Carry forward P1.2 guard corrections

**Files:**
- Modify: `scripts/mutation-test.sh`
- Test: `dependencies_test.go`
- Test: `transaction_integration_test.go`

**Step 1: Correct the dependency mutation test-first**

Change the mutation from a sibling-path replace to:

```text
replace github.com/looprig/storage => github.com/looprig/storage v0.6.0
```

Delete the obsolete sibling-checkout preflight.

**Step 2: Run the named guard test**

Run: `GOWORK=off go test -list '^TestDependencyBoundary$' ./...`

Expected: exactly one `TestDependencyBoundary` entry.

Run the self-replace mutation manually and then:

`GOWORK=off go test -run '^TestDependencyBoundary$' -count=1 ./...`

Expected behavioral failure: `go.mod has 1 replace directives, want none`.

**Step 3: Add D1 credential mutations**

Add live integration mutations replacing a lease/KV redacted failure with expressions containing `pool.Config().ConnConfig.Password` and `.User`. Both mutations target `TestOperationErrorsDoNotDiscloseDSNOrCredential` and require its exact disclosure message.

**Step 4: Defer execution until the lease error path exists**

The mutation entries are written now but executed after Task 5 supplies the production expression and integration environment.

### Task 2: Add lease timing options

**Files:**
- Modify: `options.go`
- Modify: `options_test.go`
- Modify: `pgstore.go`

**Step 1: Write failing option tests**

Add table cases proving defaults, negative durations, sub-millisecond durations, and `LeaseRenewInterval >= LeaseTTL` are rejected. Assert resolved defaults are positive and interval is strictly below TTL.

**Step 2: Verify RED**

Run: `GOWORK=off go test -run '^TestOptionsResolve$' -count=1 ./...`

Expected: compile failure because lease timing fields do not exist. This RED establishes the new API but is not mutation credit.

**Step 3: Implement minimal option resolution**

Add `LeaseTTL` and `LeaseRenewInterval` to `Options` and `resolvedOptions`, millisecond-floor validation, safe defaults, and the strict ordering rule. Pass resolved timing to `lease.New`.

**Step 4: Verify GREEN**

Run: `GOWORK=off go test -run '^TestOptionsResolve$' -count=1 ./...`

Expected: PASS.

### Task 3: Add the persistent lease schema

**Files:**
- Create: `migrations/0002_leases.sql`
- Modify: `migrations.go`
- Modify: `migrations/README.md`
- Modify: `migrations_integration_test.go`

**Step 1: Write failing migration tests**

Extend migration tests to require schema version 2 and the exact lease columns/constraints. Add an upgrade test that starts from migration 1, inserts existing Ledger/KV data, opens with `MigrationApply`, and proves data survives while the lease table appears.

**Step 2: Verify RED**

Run: `GOWORK=off go test -tags integration -run '^TestMigration(AddsLeaseTable|UpgradesVersionOneWithoutDataLoss)$' -count=1 ./...`

Expected: FAIL because version 2/table is absent.

**Step 3: Implement migration 2**

Create a persistent table keyed by lease name with signed-positive `bigint` epoch/revision, nullable holder bytes, and nullable expiry. Raise `currentSchemaVersion`, add the replacement token, and document the migration.

**Step 4: Verify GREEN**

Rerun the two named integration tests and then `GOWORK=off go test -tags integration -run '^TestMigration' -count=1 ./...`.

Expected: PASS.

### Task 4: Implement transactional acquire and release test-first

**Files:**
- Replace seam: `internal/lease/lease.go`
- Create: `internal/lease/lease_integration_test.go`
- Modify: `conformance_integration_test.go`

**Step 1: Write RED acquisition tests**

Using a disposable prefixed table through `pgxpool`, add named tests for free acquire, held error, independent keys, release, idempotent release, later epoch, concurrent first acquire, and stale release. Assert the stored row persists and epoch never resets.

**Step 2: Verify RED**

Run the named lease integration tests with `GOWORK=off go test -tags integration -run '^TestLease(Acquire|Concurrent|Release|Stale)' -count=1 ./...`.

Expected: FAIL with `*storage.NotImplementedError` from `Leaser.Acquire`.

**Step 3: Implement minimal acquire/release**

Validate deadline and name. Generate a holder token with `crypto/rand`. In a transaction, insert the persistent empty row if absent, select it `FOR UPDATE`, classify exact expiry boundaries against database time, and either return `LeaseHeldError` or advance epoch/revision and install the holder. Release conditionally clears only the matching epoch/token and closes local loss exactly once.

**Step 4: Verify GREEN**

Rerun the named tests. Expected: PASS.

### Task 5: Implement renewal, authoritative resolution, and loss test-first

**Files:**
- Modify: `internal/lease/lease.go`
- Modify: `internal/lease/lease_integration_test.go`
- Modify: `transaction_integration_test.go`

**Step 1: Write RED renewal/loss tests**

Add deterministic test hooks local to `internal/lease` for renewal and commit injection. Test successful renewal at the same epoch, exact expiry equality and one unit on either side, local expiry, later epoch, cancellation before mutation, connection turnover, restart/outage, stale renewal, ambiguous renew committed/uncommitted outcomes, and ambiguous release.

**Step 2: Verify RED**

Run each new test by exact name after confirming it with `go test -list`. Expected behavioral failures: expiry not extended, `Lost()` remaining open, or stale writer changing the row.

**Step 3: Implement minimal renewal loop**

Renew with a standalone pool update that matches name+epoch+holder+unexpired state, advances revision, and returns database expiry. On a definite compare miss, reread to distinguish expiry, release, and a later epoch, then close loss. On ambiguous acknowledgement, synchronously reread with a bounded internal context and continue only when the exact grant remains current and unexpired. Arm a conservative local expiry timer from each proved database expiry.

**Step 4: Implement store shutdown ownership**

Track live leases in the adapter so `Store.Close` cancellation ends renewal goroutines and closes their loss channels without leaking goroutines.

**Step 5: Verify GREEN and race behavior**

Run: `GOWORK=off go test -tags integration -race -run '^TestLease' -count=1 ./...`

Expected: PASS with no race report.

### Task 6: Prove transaction-pooling compatibility and advisory-lock prohibition

**Files:**
- Modify: `dependencies_test.go`
- Modify: `sql_guard_test.go`
- Modify: `internal/lease/lease_integration_test.go`
- Modify: `options.go`

**Step 1: Write source-derived RED guards**

Parse every production Go file and SQL migration. Positively assert at least one SQL-bearing source was inspected. Require exact set equality for advisory-lock occurrences: the sole allowed occurrence is the existing migration transaction lock, with exact count one. Also prohibit connection acquisition/session retention in `internal/lease`.

**Step 2: Verify RED against an injected lease advisory lock**

Add a temporary `pg_advisory_lock` string to lease production code and run the exact guard. Expected failure identifies the unauthorized occurrence. Restore before proceeding.

**Step 3: Make pool configuration transaction-pool compatible**

Move statement/lock timeouts from startup parameters to operation transactions where needed and select pgx's simple or describe execution mode that does not retain prepared-statement session state.

**Step 4: Test physical connection non-affinity**

Use a pool with multiple connections and backend-PID observations/test hooks to prove successive acquire/renew/release operations can execute on distinct physical connections while behavior remains correct.

**Step 5: Verify GREEN**

Run the exact source guard and topology tests under `-race`. Expected: PASS.

### Task 7: Run Storage conformance and close structural guards

**Files:**
- Modify: `conformance_integration_test.go`
- Modify: `deadline_guard_test.go`
- Modify: `sql_guard_test.go`
- Modify: `README.md`

**Step 1: Enable baseline conformance**

Remove the P1.3 skip from `TestLeaserConformance` and add `TestLeaserLifecycleConformance` with fresh stores, independent client views, deterministic renewal, and deterministic expiry controls.

**Step 2: Verify shared contract sufficiency**

Run both conformance entry points. They must cover baseline acquisition plus renewable lifecycle and independent clients. Provider-only restart, ambiguous acknowledgement, topology, and threshold cases remain in pgstore tests; this is complementary rather than a Storage contract gap.

**Step 3: Update source-derived guard expectations**

Remove the Leaser seam exemption, positively require all expected conformance entry points, and update README support status. Assert derived sets in both directions and exact exemption counts.

**Step 4: Verify GREEN**

Run: `GOWORK=off go test -tags integration -race -run '^TestLeaser(Conformance|LifecycleConformance)$' -count=1 ./...`

Expected: PASS.

### Task 8: Build and execute the measured mutation campaign

**Files:**
- Modify: `scripts/mutation-test.sh`
- Modify tests named by each mutation as evidence requires

**Step 1: Expand snapshot coverage**

Include every modified production/migration/test source needed by mutations. Preserve startup recovery, snapshot-before-batch, and restore-at-next-iteration behavior.

**Step 2: Add lease mutations**

Cover held/expiry comparisons at equality and either side, epoch increments, holder predicates, revision increments, persistent-row cleanup, renewal success/failure, ambiguous reread directions, release fencing, loss closure, timer boundaries, transaction row locks, operation deadlines, advisory-lock guards, conformance entry points, and D1 password/user disclosure.

**Step 3: Validate every credited test name**

For each entry the script runs `go test -list '^Name$'` before applying the mutation. Compile/setup failures are invalid and no-op mutations are not counted.

**Step 4: Run the campaign against disposable PostgreSQL**

Run: `GOWORK=off PGSTORE_TEST_DSN=... ./scripts/mutation-test.sh`

Expected: every measured mutation emits `KILLED`; zero `SURVIVED`, `INVALID`, or `WRONG_FAILURE`. Capture the measured number of `KILLED` lines rather than summing categories manually.

**Step 5: Record any unkillable assertion honestly**

If a behavior cannot be made to fail without a compile/setup failure, do not credit it. State it plainly in the result document.

### Task 9: Full verification, status report, and feature commit

**Files:**
- Create outside component repository: `../docs/plans/2026-08-29-factory-host-orchestration-implementation/CODEX_RESULT_P1.3.md`
- Review all repository-local changed files

**Step 1: Recover mutation snapshot and format**

Run recovery-only mode, `gofmt`, and `git diff --check`. Confirm no mutation remains.

**Step 2: Run required verification**

Run fresh and capture exit status/output:

```sh
GOWORK=off go test -race ./...
GOWORK=off make check
GOWORK=off go test -tags integration -race ./...
GOWORK=off go mod tidy -diff
```

All must exit zero. `go mod tidy -diff` must print no diff.

**Step 3: Write the requested status Markdown**

Write `CODEX_RESULT_P1.3.md` beside the coordinator prompt with built behavior, exact TDD record, full mutation table and failure messages, explicit unkillable assertions, conformance sufficiency, command exit statuses, HEAD, and status.

**Step 4: Commit only pgstore repository files**

Review `git diff`, stage only repository-local files, and commit:

```text
feat(pgstore): add transactional epoch leases
```

Do not push or tag. Do not stage the outer workspace result file in pgstore.

**Step 5: Verify final state freshly**

Run `git rev-parse HEAD` and `git status --short` in pgstore and update the outer result document with exact values. The expected status is clean; the outer workspace owns its result file independently.
