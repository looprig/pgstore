#!/bin/sh
set -eu

snapshot_dir="/private/tmp/pgstore-mutation-snapshot-${UID:-codex}"
# Integration entries interpolate the DSN into their argument list, which set -u
# expands before the function's own filter check can return. Default it here so a
# filtered database-free run is possible; run_integration_mutation still refuses
# to run without a real one.
PGSTORE_TEST_DSN=${PGSTORE_TEST_DSN:-}
# Repository-owned scratch. Scoped to this run and removed on every exit path,
# including an interrupt: a build cache left behind is this repository's litter,
# not the operator's.
cache_dir="/private/tmp/pgstore-gocache-$$"
completed=0

# The epilogue must be unconditional. Two separate ways of dying before it were
# measured -- a renamed test under set -e, and a DSN expanded under set -u --
# and both ended the run with no summary, no class label, and in one case an
# exit status of 0. A truncated campaign that looks like a pass is worse than
# one that fails, so the summary runs from the EXIT trap and any path that
# reaches it early is labelled ABORTED and exits nonzero.
finish() {
	trap - EXIT INT TERM
	restore_snapshot
	rm -rf "$snapshot_dir"
	rm -rf "$cache_dir"
	if test "$completed" -ne 1; then
		echo "ABORTED|the campaign exited before its epilogue after $total mutations; the results above are truncated and are not a pass"
		exit 1
	fi
	if test -n "${PGSTORE_MUTATION_EXPECTED_TOTAL:-}"; then
		expected_total=$PGSTORE_MUTATION_EXPECTED_TOTAL
		expected_source="PGSTORE_MUTATION_EXPECTED_TOTAL from the environment"
	else
		expected_total=154
		expected_source="script default"
	fi
	if test -n "${PGSTORE_MUTATION_FILTER:-}"; then
		echo "TOTAL=$total KILLED=$killed EXPECTED=(not checked: PGSTORE_MUTATION_FILTER=$PGSTORE_MUTATION_FILTER)"
		if test "$total" -eq 0; then
			echo "PGSTORE_MUTATION_FILTER=$PGSTORE_MUTATION_FILTER matched no entry; a run that measured nothing is not a pass"
			exit 1
		fi
	else
		echo "TOTAL=$total KILLED=$killed EXPECTED=$expected_total ($expected_source)"
	fi
	if test -n "$failures"; then
		echo "UNKILLED MUTATIONS:"
		printf '%s' "$failures"
		exit 1
	fi
	if test -z "${PGSTORE_MUTATION_FILTER:-}" && test "$total" -ne "$expected_total"; then
		echo "campaign ran $total mutations, want $expected_total ($expected_source): entries cannot be lost silently"
		exit 1
	fi
	echo "ALL $killed MUTATIONS KILLED"
	exit 0
}
trap 'finish' EXIT INT TERM
files="go.mod options.go pgstore.go migrations.go migrations/0002_leases.sql migrations/0003_ordered_index.sql internal/guard/guard.go internal/postgres/postgres.go internal/ledger/ledger.go internal/lease/lease.go internal/kv/kv.go internal/orderedindex/orderedindex.go internal/orderedquery/orderedquery.go conformance_integration_test.go orderedindex_plan_integration_test.go"

restore_snapshot() {
	for snapshot_file in $files; do
		if test -f "$snapshot_dir/$snapshot_file"; then
			cp "$snapshot_dir/$snapshot_file" "$snapshot_file"
		fi
	done
}

# Recover first from a prior interrupted run. This is intentionally before the
# new batch snapshot: EXIT/finally cleanup cannot run after an uncatchable kill.
if test -d "$snapshot_dir"; then
	restore_snapshot
	rm -rf "$snapshot_dir"
fi

if test "${PGSTORE_MUTATION_RECOVER_ONLY:-}" = 1; then
	exit 0
fi

mkdir -p "$snapshot_dir"
for file in $files; do
	mkdir -p "$snapshot_dir/$(dirname "$file")"
	cp "$file" "$snapshot_dir/$file"
done

# Failure accounting. A single stale expectation used to abort the campaign,
# which silently removed every mutation after it from the run: at HEAD~ that hid
# 35 of them, including all four OrderedIndex credential-leak mutations. A
# mutation that cannot be classified is now recorded and the campaign continues,
# so one stale string costs one mutation and is reported beside every other
# result instead of truncating the evidence.
total=0
killed=0
failures=""

record_failure() {
	failures="$failures$1
"
}

run_mutation() {
	name=$1
	file=$2
	old=$3
	new=$4
	test_name=$5
	want=$6
	if test -n "${PGSTORE_MUTATION_FILTER:-}" && test "$name" != "$PGSTORE_MUTATION_FILTER"; then
		return
	fi

	# The previous mutation is restored at the next iteration's entry. If this
	# process is killed mid-test, the startup recovery above performs this step.
	restore_snapshot
	total=$((total + 1))
	# A renamed or deleted test must be classified, not fatal. Under set -e the
	# failing grep in this pipeline used to kill the whole script mid-run, with
	# no class label and no epilogue -- the same silent truncation the failure
	# accounting above exists to prevent, for the one trigger it did not cover.
	# A test rename is at least as likely as a message rewrite.
	if ! GOWORK=off GOCACHE="$cache_dir" go test -list "^${test_name}$" ./... 2>/dev/null | grep -qx "$test_name"; then
		echo "MISSING_TEST|$name|$test_name|no test with this name exists"
		record_failure "MISSING_TEST|$name|$test_name"
		return
	fi
	if ! grep -Fq "$old" "$file"; then
		echo "DRIFTED|$name|$test_name|mutation pattern no longer present in $file"
		record_failure "DRIFTED|$name"
		return
	fi
	MUT_OLD=$old MUT_NEW=$new perl -0pi -e 's/\Q$ENV{"MUT_OLD"}\E/$ENV{"MUT_NEW"}/' "$file"

	set +e
	output=$(GOWORK=off GOCACHE="$cache_dir" go test -run "^${test_name}$" -count=1 ./... 2>&1)
	status=$?
	set -e
	if test "$status" -eq 0; then
		echo "SURVIVED|$name|$test_name|test passed"
		record_failure "SURVIVED|$name"
		return
	fi
	if printf '%s\n' "$output" | grep -Eq '\[build failed\]|\[setup failed\]|undefined:|syntax error'; then
		echo "INVALID|$name|$test_name|compile/setup failure"
		printf '%s\n' "$output"
		record_failure "INVALID|$name"
		return
	fi
	if ! printf '%s\n' "$output" | grep -Fq "$want"; then
		echo "WRONG_FAILURE|$name|$test_name|missing: $want"
		printf '%s\n' "$output"
		record_failure "WRONG_FAILURE|$name"
		return
	fi
	killed=$((killed + 1))
	echo "KILLED|$name|$test_name|$want"
}

run_integration_mutation() {
	name=$1
	file=$2
	old=$3
	new=$4
	test_name=$5
	want=$6
	if test -n "${PGSTORE_MUTATION_FILTER:-}" && test "$name" != "$PGSTORE_MUTATION_FILTER"; then
		return
	fi

	restore_snapshot
	PGSTORE_TEST_DSN=${PGSTORE_TEST_DSN:?PGSTORE_TEST_DSN is required for P1.2 mutations}
	export PGSTORE_TEST_DSN
	total=$((total + 1))
	if ! GOWORK=off GOCACHE="$cache_dir" go test -tags integration -list "^${test_name}$" ./... 2>/dev/null | grep -qx "$test_name"; then
		echo "MISSING_TEST|$name|$test_name|no test with this name exists"
		record_failure "MISSING_TEST|$name|$test_name"
		return
	fi
	if ! grep -Fq "$old" "$file"; then
		echo "DRIFTED|$name|$test_name|mutation pattern no longer present in $file"
		record_failure "DRIFTED|$name"
		return
	fi
	MUT_OLD=$old MUT_NEW=$new perl -0pi -e 's/\Q$ENV{"MUT_OLD"}\E/$ENV{"MUT_NEW"}/' "$file"

	set +e
	output=$(GOWORK=off GOCACHE="$cache_dir" go test -tags integration -run "^${test_name}$" -count=1 ./... 2>&1)
	status=$?
	set -e
	if test "$status" -eq 0; then
		echo "SURVIVED|$name|$test_name|test passed"
		record_failure "SURVIVED|$name"
		return
	fi
	if printf '%s\n' "$output" | grep -Eq '\[build failed\]|\[setup failed\]|undefined:|syntax error'; then
		echo "INVALID|$name|$test_name|compile/setup failure"
		printf '%s\n' "$output"
		record_failure "INVALID|$name"
		return
	fi
	if ! printf '%s\n' "$output" | grep -Fq "$want"; then
		echo "WRONG_FAILURE|$name|$test_name|missing: $want"
		printf '%s\n' "$output"
		record_failure "WRONG_FAILURE|$name"
		return
	fi
	killed=$((killed + 1))
	echo "KILLED|$name|$test_name|$want"
}

run_mutation "missing DSN" options.go 'if strings.TrimSpace(o.DSN) == "" {' 'if false && strings.TrimSpace(o.DSN) == "" {' TestOptionsResolve 'want substring "must be set"'
run_mutation "DSN redaction" options.go 'invalidOption("DSN", "invalid PostgreSQL connection string")' 'invalidOption("DSN", "invalid PostgreSQL connection string: "+o.DSN)' TestOptionsErrorNeverUnwrapsDSNParserError 'error disclosed credential'
run_mutation "verified TLS" options.go 'config.ConnConfig.TLSConfig != nil && !config.ConnConfig.TLSConfig.InsecureSkipVerify' 'config.ConnConfig.TLSConfig != nil' TestOptionsResolve 'unverified_TLS_DSN'
run_mutation "remote plaintext" options.go 'isLoopbackHost(config.ConnConfig.Host)' 'true' TestOptionsResolveRejectsExplicitRemoteInsecureTransport 'want loopback-only'
run_mutation "localhost classification" options.go 'if strings.EqualFold(host, "localhost") {' 'if false && strings.EqualFold(host, "localhost") {' TestOptionsResolveAllowsExplicitLocalInsecureTransport 'must require verified TLS'
run_mutation "loopback IP classification" options.go 'return ip != nil && ip.IsLoopback()' 'return false && ip != nil && ip.IsLoopback()' TestOptionsResolveAllowsLoopbackIPInsecureTransport 'must require verified TLS'
run_mutation "maximum nonnegative" options.go 'if maxConns < 0 {' 'if false && maxConns < 0 {' TestOptionsResolve 'maximum_connections_negative'
run_mutation "maximum default" options.go 'if maxConns == 0 {' 'if false && maxConns == 0 {' TestOptionsResolve 'resolved defaults'
run_mutation "minimum nonnegative" options.go 'if o.MinConns < 0 {' 'if false && o.MinConns < 0 {' TestOptionsResolve 'minimum_connections_negative'
run_mutation "minimum bounded by maximum" options.go 'if o.MinConns > maxConns {' 'if false && o.MinConns > maxConns {' TestOptionsResolve 'minimum_exceeds_maximum'
run_mutation "schema default" options.go 'if schema == "" {' 'if false && schema == "" {' TestOptionsResolve 'valid_defaults'
run_mutation "table prefix default" options.go 'if tablePrefix == "" {' 'if false && tablePrefix == "" {' TestOptionsResolve 'valid_defaults'
run_mutation "identifier byte bound" options.go 'len(value) > maxBytes' 'false && len(value) > maxBytes' TestOptionsResolve 'oversized_schema'
run_mutation "identifier first byte" options.go "value[0] < 'a' || value[0] > 'z'" "false && (value[0] < 'a' || value[0] > 'z')" TestOptionsResolve 'uppercase_schema'
run_mutation "identifier remaining bytes" options.go "if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '_' {" 'if char == char && false {' TestOptionsResolve 'invalid_schema'
run_mutation "negative timeout" options.go 'if requested < 0 {' 'if false && requested < 0 {' TestOptionsResolve 'want substring "must be positive"'
run_mutation "timeout default" options.go 'if requested == 0 {' 'if false && requested == 0 {' TestOptionsResolve 'valid_defaults'
run_mutation "timeout millisecond floor" options.go 'if requested < time.Millisecond {' 'if false && requested < time.Millisecond {' TestOptionsResolve 'sub-millisecond_statement_timeout'
run_mutation "lock bounded by statement" options.go 'if lockTimeout > statementTimeout {' 'if false && lockTimeout > statementTimeout {' TestOptionsResolve 'lock_timeout_exceeds_statement_timeout'
run_mutation "renew interval bounded by lease TTL" options.go 'if leaseRenewInterval >= leaseTTL {' 'if false && leaseRenewInterval >= leaseTTL {' TestOptionsResolve 'lease_renew_interval_equals_TTL'
run_mutation "transaction-pool query mode" options.go 'poolConfig.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec' 'poolConfig.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement' TestOptionsResolveAppliesValidCustomValues 'want QueryExecModeExec'
run_mutation "transaction-pool startup parameter" options.go 'delete(poolConfig.ConnConfig.RuntimeParams, "statement_timeout")' 'poolConfig.ConnConfig.RuntimeParams["statement_timeout"] = "1"' TestOptionsResolveAppliesValidCustomValues 'statement_timeout is a startup RuntimeParam'
run_mutation "migration enum" options.go 'if o.Migrations > MigrationDisabled {' 'if false && o.Migrations > MigrationDisabled {' TestOptionsResolve 'invalid_migration_mode'
run_mutation "nil context" internal/guard/guard.go 'if ctx == nil {' 'if false && ctx == nil {' TestOperationRejectsNilContext 'panic:'
run_mutation "context deadline" internal/guard/guard.go 'if _, ok := ctx.Deadline(); !ok {' 'if _, ok := ctx.Deadline(); ok && false {' TestOpenRequiresDeadline 'want *DeadlineRequiredError'
run_mutation "Open option short circuit" pgstore.go 'if err != nil {' 'if false && err != nil {' TestOpenRejectsOptionsBeforePoolConstruction 'panic:'
run_mutation "Open deadline" pgstore.go 'guard.RequireDeadline(ctx, "Open")' 'guard.NotImplemented("Open")' TestOpenRequiresDeadline 'want *DeadlineRequiredError'
run_mutation "pool error redaction" pgstore.go 'invalidOption("DSN", "PostgreSQL pool configuration was rejected")' 'invalidOption("DSN", "PostgreSQL pool configuration was rejected: "+err.Error())' TestOpenRedactsPoolConstructionError 'want non-unwrapping redacted error'
run_mutation "nil Close" pgstore.go 'if s == nil {' 'if false && s == nil {' TestStoreCloseIsNilSafeAndIdempotent 'panic:'
run_mutation "idempotent Close" pgstore.go 's.closeOnce.Do(s.closePool)' 's.closePool()' TestStoreCloseIsNilSafeAndIdempotent 'pool close calls = 2'
run_mutation "local replace directive" go.mod 'go 1.26.6' 'go 1.26.6

replace github.com/looprig/storage => github.com/looprig/storage v0.6.0' TestDependencyBoundary 'go.mod has 1 replace directives, want none'
run_mutation "extra direct module" go.mod 'github.com/jackc/puddle/v2 v2.2.2 // indirect' 'github.com/jackc/puddle/v2 v2.2.2' TestDependencyBoundary 'direct modules ='
run_mutation "logging import" pgstore.go '"context"' '"context"
	_ "log/slog"' TestDependencyBoundary 'imports logging package "log/slog"'
run_mutation "Blobs field" pgstore.go 'Ledger       storage.Ledger' 'Blobs        storage.Blobs
	Ledger       storage.Ledger' TestOpenWiresStructuredPrimitivesWithoutBlobs 'Store exposes a Blobs field'

for operation in Leaser.Acquire Lease.Release OrderedIndex.Get OrderedIndex.Create OrderedIndex.Update OrderedIndex.Delete OrderedIndex.ListOrdered OrderedIndex.ListRanked OrderedIndex.ListDue; do
	case $operation in
		Ledger.*) file=internal/ledger/ledger.go ;;
		Leaser.*) file=internal/lease/lease.go ;;
		Lease.*) file=internal/lease/lease.go ;;
		KV.*) file=internal/kv/kv.go ;;
		OrderedIndex.*) file=internal/orderedindex/orderedindex.go ;;
	esac
	run_mutation "$operation deadline call" "$file" "guard.RequireDeadline(ctx, \"$operation\")" "guard.NotImplemented(\"$operation\")" TestStructuredOperationMethodsCallDeadlineGuard 'does not call guard.RequireDeadline'
done

run_mutation "retry SQLSTATE classification" internal/postgres/postgres.go 'pgErr.Code == "40001"' 'pgErr.Code == "40002"' TestRetryableClassifiesOnlySerializationAndDeadlockSQLStates 'Retryable(SQLSTATE 40001) = false, want true'
run_mutation "lease advisory lock prohibited" internal/lease/lease.go 'const holderTokenBytes = 32' 'const holderTokenBytes = 32
const forbiddenLeaseSQL = "SELECT pg_advisory_lock(1)"' TestProductionAdvisoryLockOccurrencesAreExactlyTheMigrationLock 'advisory-lock source set ='
run_mutation "lease acquire row lock" internal/lease/lease.go ' WHERE name = $1 FOR UPDATE' ' WHERE name = $1' TestLeaseAcquireUsesExactlyOneExplicitRowLock 'FOR UPDATE occurrences = 0, want exactly 1'
run_mutation "lease physical connection retention" internal/lease/lease.go 'var _ storage.Leaser = (*Store)(nil)' 'func (s *Store) retainedConnection(ctx context.Context) (*pgxpool.Conn, error) { return s.pool.Acquire(ctx) }

var _ storage.Leaser = (*Store)(nil)' TestLeaseStoreDoesNotRetainPhysicalPoolConnections 'lease store contains retained physical-connection form'
run_mutation "lease proof latency" internal/lease/lease.go 'return expiresAt.Sub(databaseNow) - time.Since(proofStarted)' 'return expiresAt.Sub(databaseNow) + 0*time.Since(proofStarted)' TestConservativeRemainingSubtractsTheWholeProofDuration 'want nonpositive after proof latency crossed expiry'
run_integration_mutation "lease held expiry comparison" internal/lease/lease.go 'currentHolder != nil && expiresAt != nil && expiresAt.After(databaseNow)' 'currentHolder != nil && expiresAt != nil && !expiresAt.After(databaseNow)' TestLeaseExpiryComparisonAtAndAroundThreshold 'want held at epoch'
run_integration_mutation "lease acquire epoch increment" internal/lease/lease.go 'epoch++' 'epoch += 0' TestLeaseAcquireHeldReleaseAndLaterEpoch 'want live epoch 1'
run_integration_mutation "lease acquire revision increment" internal/lease/lease.go 'SET epoch = $2, holder = $3, expires_at = clock_timestamp() + $4::bigint * interval '"'"'1 millisecond'"'"', revision = revision + 1 WHERE name = $1' 'SET epoch = $2, holder = $3, expires_at = clock_timestamp() + $4::bigint * interval '"'"'1 millisecond'"'"', revision = revision WHERE name = $1' TestLeaseAcquireHeldReleaseAndLaterEpoch 'persistent row ='
run_integration_mutation "lease acquire authoritative reread" internal/lease/lease.go 'return s.resolveAcquire(name, epoch, holder)' 'return nil, pginternal.RedactedError("lease acquire")' TestLeaseAcquireResolvesCommittedLostAcknowledgement 'Acquire after committed lost acknowledgement:'
run_integration_mutation "lease acquire unresolved outcome fails closed" internal/lease/lease.go 'return nil, pginternal.RedactedError("lease acquire outcome resolution")' 'return s.newLease(name, epoch, holder, s.leaseTTL), nil' TestLeaseAcquireRejectsUncommittedLostAcknowledgement 'want no lease and error'
run_integration_mutation "lease acquire local expiry uses conservative proof" internal/lease/lease.go 'return s.newLease(name, epoch, holder, conservativeRemaining(proofStarted, *expiresAt, databaseNow)), nil' 'return s.newLease(name, epoch, holder, expiresAt.Sub(databaseNow)+0*time.Since(proofStarted)), nil' TestLeaseLocalDeadlineAccountsForCommitLatency 'Lost after commit latency crossed the database expiry remained open'
run_integration_mutation "lease acquire transaction-local timeouts" internal/lease/lease.go 'if err := pginternal.SetLocalTimeouts(ctx, tx, s.statementTimeout, s.lockTimeout); err != nil {
		return nil, leaseFailure(ctx, "lease acquire")
	}' 'if false {
		return nil, leaseFailure(ctx, "lease acquire")
	}' TestLeaseAcquireAppliesTransactionLocalLockTimeout 'want transaction-local 50ms lock timeout'
run_integration_mutation "lease renew epoch predicate" internal/lease/lease.go 'AND epoch = $2 AND holder = $3 AND expires_at > clock_timestamp() RETURNING' 'AND epoch = $2 + 1 AND holder = $3 AND expires_at > clock_timestamp() RETURNING' TestLeaseEpochAndHolderFencesAtAndAroundThreshold 'renew at epoch 1 = <nil>, want pgx.ErrNoRows'
run_integration_mutation "lease renew holder predicate" internal/lease/lease.go 'AND epoch = $2 AND holder = $3 AND expires_at > clock_timestamp() RETURNING' 'AND epoch = $2 AND holder <> $3 AND expires_at > clock_timestamp() RETURNING' TestLeaseEpochAndHolderFencesAtAndAroundThreshold 'renew with wrong holder = <nil>, want pgx.ErrNoRows'
run_integration_mutation "lease renew expiry predicate" internal/lease/lease.go 'AND holder = $3 AND expires_at > clock_timestamp() RETURNING' 'AND holder = $3 RETURNING' TestLeaseExpiryClosesLostAndLaterGrantAdvancesEpoch 'Lost after database expiry remained open'
run_integration_mutation "lease renew revision increment" internal/lease/lease.go 'SET expires_at = clock_timestamp() + $4::bigint * interval '"'"'1 millisecond'"'"', revision = revision + 1 WHERE name = $1 AND epoch' 'SET expires_at = clock_timestamp() + $4::bigint * interval '"'"'1 millisecond'"'"', revision = revision WHERE name = $1 AND epoch' TestLeaseEpochAndHolderFencesAtAndAroundThreshold 'exact writer revision ='
run_integration_mutation "ambiguous renew skips authoritative reread" internal/lease/lease.go 'result, err = l.store.observe(proofCtx, l)' 'result, err = renewal{remaining: l.store.leaseTTL}, nil
			_ = proofCtx' TestLeaseAmbiguousRenewClosesLostUnlessAuthoritativeReadProvesOwnership 'Lost after ambiguous renewal observed later epoch remained open'
run_integration_mutation "lease release epoch predicate" internal/lease/lease.go 'WHERE name = $1 AND epoch = $2 AND holder = $3", lease.name' 'WHERE name = $1 AND epoch = $2 + 1 AND holder = $3", lease.name' TestLeaseReleaseRequiresExactEpochAndHolder 'stale release changed row'
run_integration_mutation "lease release holder predicate" internal/lease/lease.go 'WHERE name = $1 AND epoch = $2 AND holder = $3", lease.name' 'WHERE name = $1 AND epoch = $2 AND holder <> $3", lease.name' TestLeaseReleaseRequiresExactEpochAndHolder 'stale release changed row'
run_integration_mutation "lease release resets epoch" internal/lease/lease.go 'SET holder = NULL, expires_at = NULL, revision = revision + 1 WHERE name' 'SET epoch = 0, holder = NULL, expires_at = NULL, revision = revision + 1 WHERE name' TestLeaseAcquireHeldReleaseAndLaterEpoch 'later epoch = 1, want 2'
run_integration_mutation "lease release freezes revision" internal/lease/lease.go 'SET holder = NULL, expires_at = NULL, revision = revision + 1 WHERE name' 'SET holder = NULL, expires_at = NULL, revision = revision WHERE name' TestLeaseReleaseRequiresExactEpochAndHolder 'exact release revision ='
run_integration_mutation "lease release omits Lost closure" internal/lease/lease.go '		l.stop()
		l.markLost()
		l.store.unregister(l)' '		l.stop()
		l.store.unregister(l)' TestLeaseAcquireHeldReleaseAndLaterEpoch 'Lost remained open after Release'
run_integration_mutation "lease canceled release cannot retry" internal/lease/lease.go 'if l.releaseComplete {' 'if l.released {' TestLeaseCanceledReleaseCanBeRetried 'Acquire after release retry:'
run_integration_mutation "lease local expiry timer" internal/lease/lease.go 'case <-expiryTimer.C:' 'case <-time.After(24 * time.Hour):' TestLeaseLocalDeadlineClosesLostBeforeRenewTick 'Lost at the locally tracked database expiry remained open'
run_integration_mutation "lease release authoritative reread" internal/lease/lease.go 'l.store.resolveRelease(l)' 'pginternal.RedactedError("lease release")' TestLeaseReleaseResolvesCommittedLostAcknowledgement 'Release after committed lost acknowledgement:'
run_integration_mutation "lease release unresolved outcome fails closed" internal/lease/lease.go 'if epoch != lease.epoch || !bytes.Equal(holder, lease.holder) || expiresAt == nil || !expiresAt.After(databaseNow) {' 'if true || epoch != lease.epoch || !bytes.Equal(holder, lease.holder) || expiresAt == nil || !expiresAt.After(databaseNow) {' TestLeaseReleaseRejectsUncommittedLostAcknowledgementAndRetries 'Release after uncommitted acknowledgement = nil error'
run_integration_mutation "lease migration primary key" migrations/0002_leases.sql 'name text PRIMARY KEY,' 'name text UNIQUE NOT NULL,' TestMigrationAddsLeaseTable 'lease constraints ='
run_integration_mutation "lease migration epoch constraint" migrations/0002_leases.sql 'epoch bigint NOT NULL CHECK (epoch >= 0),' 'epoch bigint NOT NULL,' TestMigrationAddsLeaseTable 'lease constraints ='
run_integration_mutation "lease migration revision constraint" migrations/0002_leases.sql 'revision bigint NOT NULL CHECK (revision >= 0),' 'revision bigint NOT NULL,' TestMigrationAddsLeaseTable 'lease constraints ='
run_integration_mutation "lease migration holder expiry parity" migrations/0002_leases.sql 'CHECK ((holder IS NULL) = (expires_at IS NULL))' 'CHECK (true)' TestMigrationAddsLeaseTable 'lease constraints ='
run_integration_mutation "migration explicit lock" migrations.go 'SELECT pg_advisory_xact_lock(hashtextextended($1, 0))' 'SELECT $1::text' TestConcurrentMigrationOwnersSerializeFromVersionZero 'concurrent Open:'
run_integration_mutation "ledger per-scope row lock" internal/ledger/ledger.go ' WHERE name = $1 FOR UPDATE' ' WHERE name = $1' TestLedgerConformance 'writer reported error:'
run_integration_mutation "ledger commit authoritative reread" internal/ledger/ledger.go 'if errors.As(err, &commitErr) {' 'if false && errors.As(err, &commitErr) {' TestAppendResolvesCommitAcknowledgementLossByAuthoritativeRead 'want nil after authoritative reread'
run_integration_mutation "ledger delete authoritative reread" internal/ledger/ledger.go 'return s.resolveDelete(name, safeCause(ctx, "ledger delete"))' 'return operationFailure(ctx, "ledger delete")' TestAppendResolvesCommitAcknowledgementLossByAuthoritativeRead 'Delete after committed lost acknowledgement:'
run_integration_mutation "ledger retry cancellation" internal/ledger/ledger.go 'if ctxErr := ctx.Err(); ctxErr != nil {' 'if ctxErr := ctx.Err(); false && ctxErr != nil {' TestAppendNeverRetriesSerializationFailureAfterCallerCancellation 'transaction attempts = 2, want 1'
run_integration_mutation "kv put authoritative reread" internal/kv/kv.go 'return s.resolvePut(key, expected, value, safeCause(ctx, "kv put"))' 'return 0, pginternal.RedactedError("kv put")' TestPutAndDeleteResolveLostAcknowledgementsThroughPublicAPI 'want (1, nil) after reread'
run_integration_mutation "kv delete authoritative reread" internal/kv/kv.go 'return s.resolveDelete(key, safeCause(ctx, "kv delete"))' 'return failure(ctx, "kv delete")' TestPutAndDeleteResolveLostAcknowledgementsThroughPublicAPI 'Delete after lost ack:'
run_integration_mutation "kv absent outcome" internal/kv/kv.go 'if err == pgx.ErrNoRows || (err == nil && revision == expected) {' 'if false || (err == nil && revision == expected) {' TestResolvePutAbsentProvesCanceledCreateDidNotCommit 'want original cancellation'
run_integration_mutation "kv CAS revision predicate" internal/kv/kv.go ' WHERE key = $1 AND revision = $2 RETURNING revision' ' WHERE key = $1 RETURNING revision' TestConcurrentKVCASHasExactlyOneWinnerPerRevision 'want exactly 1'
run_integration_mutation "kv prefix parameterization" internal/kv/kv.go '"SELECT key FROM "+s.table()+" WHERE left(key, length($1)) = $1 ORDER BY key COLLATE \"C\"", prefix' '"SELECT key FROM "+s.table()+" WHERE left(key, length('"'"'"+prefix+"'"'"')) = '"'"'"+prefix+"'"'"' ORDER BY key COLLATE \"C\""' TestKVKeysPrefixIsDataNotSQL 'Keys with SQL-shaped prefix:'
run_mutation "kv bytewise collation" internal/kv/kv.go 'ORDER BY key COLLATE \"C\"' 'ORDER BY key' TestKVKeysPinsBytewiseCollation 'does not pin ORDER BY'

# P1.4 OrderedIndex behavioral mutation matrix.
run_integration_mutation "ordered stable key migration bytes" migrations/0003_ordered_index.sql 'stable_key bytea NOT NULL' 'stable_key text COLLATE "C" NOT NULL' TestMigrationAddsOrderedIndexTablesAndExactIndexes 'stable_key type = "text", want bytea'
run_integration_mutation "ordered stable key SQL bytes" internal/orderedindex/orderedindex.go 'return []byte(string(key))' 'return []byte(string(key[1:]))' TestEmbeddedNULStableKeysRoundTripAndPageBytewise 'Get("\x00")'
run_mutation "ordered explicit Read Committed isolation" internal/orderedindex/orderedindex.go 'pgx.TxOptions{IsoLevel: pgx.ReadCommitted}' 'pgx.TxOptions{IsoLevel: pgx.Serializable}' TestOrderedIndexMutationsPinReadCommittedAndExplicitRowLocks 'so it must run at pgx.ReadCommitted'
run_integration_mutation "ordered counter increment" internal/orderedindex/orderedindex.go 'SET next_order = next_order + 1 WHERE namespace' 'SET next_order = next_order + 0 WHERE namespace' TestOrderedIndexConformance 'want record, true, nil'
run_integration_mutation "ordered counter row lock" internal/orderedindex/orderedindex.go 'ordering_scope = $2 FOR UPDATE", id.Namespace' 'ordering_scope = $2", id.Namespace' TestConcurrentDuplicateConsumesExactlyOneCounterValue 'concurrent duplicate:'
run_integration_mutation "ordered duplicate recheck after counter lock" internal/orderedindex/orderedindex.go 'if lockedExisting, readErr := s.getFrom(ctx, tx, id, ""); readErr == nil {' 'if lockedExisting, readErr := s.getFrom(ctx, tx, id, ""); false && readErr == nil {' TestConcurrentDuplicateConsumesExactlyOneCounterValue 'concurrent duplicate'
run_integration_mutation "ordered update authoritative row lock" internal/orderedindex/orderedindex.go 'return s.getFrom(ctx, tx, id, " FOR UPDATE")' 'return s.getFrom(ctx, tx, id, "")' TestConcurrentUpdateCASHasExactlyOneWinner 'want 1/15'
run_integration_mutation "ordered delete authoritative row lock" internal/orderedindex/orderedindex.go 'return s.getFrom(ctx, tx, id, " FOR UPDATE")' 'return s.getFrom(ctx, tx, id, "")' TestConcurrentDeleteAdvancesRevisionExactlyOnce 'want true/2'
run_integration_mutation "ordered atomic rank move" internal/orderedindex/orderedindex.go 'ranked = $6, rank_value = $7' 'ranked = NOT $6, rank_value = $7' TestOrderedIndexConformance 'Rank false/99'
run_integration_mutation "ordered atomic due move" internal/orderedindex/orderedindex.go 'due_state = $8, due_at = $9' 'due_state = $8, due_at = $9 + 1' TestOrderedIndexConformance 'Due 1/-4'
run_integration_mutation "ordered order keyset direction" internal/orderedquery/orderedquery.go 'order_id > $3::numeric' 'order_id < $3::numeric' TestOrderedIndexConformance 'ListOrdered'
run_integration_mutation "ordered ranked keyset direction" internal/orderedquery/orderedquery.go '(rank_value, stable_key, ordering_scope) < ($3, $4, $5)' '(rank_value, stable_key, ordering_scope) > ($3, $4, $5)' TestOrderedIndexConformance 'ListRanked'
run_integration_mutation "ordered due keyset direction" internal/orderedquery/orderedquery.go '(due_at, stable_key, ordering_scope) > ($3, $4, $5)' '(due_at, stable_key, ordering_scope) < ($3, $4, $5)' TestOrderedIndexConformance 'ListDue'
run_mutation "ordered ranked total tie break" internal/orderedquery/orderedquery.go 'ORDER BY rank_value DESC, stable_key DESC, ordering_scope DESC' 'ORDER BY rank_value DESC, stable_key DESC' TestOrderedIndexQueriesRemainIndexBackedKeysets 'lost required keyset fragment'
run_mutation "ordered due total tie break" internal/orderedquery/orderedquery.go 'ORDER BY due_at ASC, stable_key ASC, ordering_scope ASC' 'ORDER BY due_at ASC, stable_key ASC' TestOrderedIndexQueriesRemainIndexBackedKeysets 'lost required keyset fragment'
run_integration_mutation "ordered delete tombstone" internal/orderedindex/orderedindex.go 'due_at = 0, deleted = true WHERE namespace' 'due_at = 0, deleted = false WHERE namespace' TestOrderedIndexConformance 'omitted its tombstone state'
run_integration_mutation "ordered delete clears rank" internal/orderedindex/orderedindex.go 'ranked = false, rank_value = 0' 'ranked = ranked, rank_value = rank_value' TestOrderedIndexConformance 'Delete('
run_mutation "ordered cursor canonical encoding" internal/orderedindex/orderedindex.go '!bytes.Equal(canonical, raw)' 'false && !bytes.Equal(canonical, raw)' TestCursorDecoderRejectsOversizeAndNoncanonicalTokens 'want ranked malformed cursor'
run_mutation "ordered cursor version" internal/orderedindex/orderedindex.go 'if *header.Version != cursorVersion {' 'if false && *header.Version != cursorVersion {' TestCursorDecoderClassifiesVersionKindAndQueryBeforeUse 'want ranked/unknown version'
run_mutation "ordered ranked cursor query binding" internal/orderedindex/orderedindex.go 'envelope.RankingScope != scope' 'false && envelope.RankingScope != scope' TestCursorDecoderClassifiesVersionKindAndQueryBeforeUse 'want ranked/query mismatch'
run_mutation "ordered due cursor bound binding" internal/orderedindex/orderedindex.go '*envelope.DueBound != bound' 'false && *envelope.DueBound != bound' TestCursorDecoderClassifiesVersionKindAndQueryBeforeUse 'want due/query mismatch'
run_integration_mutation "ordered ambiguity rejects later revision" internal/orderedindex/orderedindex.go 'return recordsEqual(got, want)' 'return true' TestCommitAcknowledgementLossResolvesOnlyExactPostState 'want update ambiguity'
run_integration_mutation "ordered ranked namespace isolation" internal/orderedquery/orderedquery.go 'WHERE namespace = $1 AND ranking_scope = $2' 'WHERE ranking_scope = $2' TestListViewsIsolateNamespaceAndOrderingScope 'namespace isolation'
run_integration_mutation "ordered due namespace isolation" internal/orderedquery/orderedquery.go 'WHERE namespace = $1 AND due_state = 1' 'WHERE due_state = 1' TestListViewsIsolateNamespaceAndOrderingScope 'namespace isolation'
run_integration_mutation "ordered migration direct identity" migrations/0003_ordered_index.sql 'PRIMARY KEY (namespace, ordering_scope, stable_key),' 'UNIQUE (namespace, ordering_scope, stable_key),' TestMigrationAddsOrderedIndexTablesAndExactIndexes 'index exact_ordered_records_pkey'
run_integration_mutation "ordered migration canonical due" migrations/0003_ordered_index.sql 'CHECK ((due_state = 0 AND due_at = 0) OR due_state = 1),' 'CHECK (true),' TestMigrationAddsOrderedIndexTablesAndExactIndexes 'accepted noncanonical due'
run_integration_mutation "ordered migration tombstone constraint" migrations/0003_ordered_index.sql 'CHECK (NOT deleted OR (NOT ranked AND due_state = 0 AND due_at = 0))' 'CHECK (true)' TestMigrationAddsOrderedIndexTablesAndExactIndexes 'accepted active tombstone'
run_integration_mutation "ordered migration value bound" migrations/0003_ordered_index.sql 'octet_length(value) <= 1048576' 'octet_length(value) <= 1048577' TestMigrationAddsOrderedIndexTablesAndExactIndexes 'accepted oversized value'
run_integration_mutation "ordered intended rank plan" migrations/0003_ordered_index.sql 'rank_value DESC, stable_key DESC, ordering_scope DESC' 'rank_value ASC, stable_key DESC, ordering_scope DESC' TestOrderedIndexPlansUseExactKeysetIndexesForFirstAndMiddlePages 'required an explicit sort'
run_integration_mutation "ordered due index has no invented scope" migrations/0003_ordered_index.sql 'namespace, due_state, due_at, stable_key, ordering_scope' 'namespace, due_state, ordering_scope, due_at, stable_key' TestOrderedIndexPlansUseExactKeysetIndexesForFirstAndMiddlePages 'did not select intended index'
run_integration_mutation "ordered production order plan" internal/orderedquery/orderedquery.go 'ORDER BY " + table + ".order_id ASC, stable_key ASC' 'ORDER BY order_id ASC, stable_key ASC' TestOrderedIndexPlansUseExactKeysetIndexesForFirstAndMiddlePages 'required an explicit sort'
run_integration_mutation "ordered production ranked argument order" internal/orderedquery/orderedquery.go 'position.Rank, position.StableKey, position.OrderingScope' 'position.Rank, position.OrderingScope, position.StableKey' TestOrderedIndexPlansUseExactKeysetIndexesForFirstAndMiddlePages 'want exact values/types/order'
run_mutation "ordered plan gate copied ordered SQL" orderedindex_plan_integration_test.go 'statement: orderedquery.Ordered(table, "sessions", "scope-1", 0, 25)' 'statement: orderedquery.Statement{SQL: "SELECT " + orderedquery.RecordColumns + " FROM " + table + " WHERE namespace = $1 AND ordering_scope = $2 AND order_id " + "> $3::numeric ORDER BY " + table + ".order_id ASC, stable_key ASC LIMIT $4", Args: []any{"sessions", "scope-1", "0", 25}}' TestOrderedIndexPlanGateUsesProductionStatements 'statement is not a direct orderedquery page-builder call'
run_mutation "ordered plan gate copied ranked SQL" orderedindex_plan_integration_test.go 'statement: orderedquery.Ranked(table, "sessions", "workers", nil, 24)' 'statement: orderedquery.Statement{SQL: "SELECT " + orderedquery.RecordColumns + " FROM " + table + " WHERE namespace = $1 AND ranking_scope = $2 AND ranked AND NOT deleted ORDER BY rank_value DESC, stable_key DESC, ordering_scope DESC LIMIT $3", Args: []any{"sessions", "workers", 25}}' TestOrderedIndexPlanGateUsesProductionStatements 'statement is not a direct orderedquery page-builder call'
run_mutation "ordered plan gate copied due SQL" orderedindex_plan_integration_test.go 'statement: orderedquery.Due(table, "sessions", 999, nil, 24)' 'statement: orderedquery.Statement{SQL: "SELECT " + orderedquery.RecordColumns + " FROM " + table + " WHERE namespace = $1 AND due_state = 1 AND NOT deleted AND due_at <= $2 ORDER BY due_at ASC, stable_key ASC, ordering_scope ASC LIMIT $3", Args: []any{"sessions", int64(999), 25}}' TestOrderedIndexPlanGateUsesProductionStatements 'statement is not a direct orderedquery page-builder call'
run_mutation "ordered production dead second receiver query" internal/orderedindex/orderedindex.go 'statement := orderedquery.Ranked(s.recordsTable(), namespace, rankingScope, queryPosition, limit)
	rows, err := s.pool.Query(ctx, statement.SQL, statement.Args...)' 'statement := orderedquery.Ranked(s.recordsTable(), namespace, rankingScope, queryPosition, limit)
	if false { decoyRows, decoyErr := s.pool.Query(ctx, "SELECT 1"); if decoyErr == nil { decoyRows.Close() } }
	rows, err := s.pool.Query(ctx, statement.SQL, statement.Args...)' TestOrderedIndexPlanGateUsesProductionStatements 'The page must come from the one orderedquery statement, so exactly one Query is allowed.'
run_mutation "ordered production copied live query with unused builder" internal/orderedindex/orderedindex.go 'statement := orderedquery.Ranked(s.recordsTable(), namespace, rankingScope, queryPosition, limit)
	rows, err := s.pool.Query(ctx, statement.SQL, statement.Args...)' 'statement := orderedquery.Ranked(s.recordsTable(), namespace, rankingScope, queryPosition, limit)
	_ = statement
	rows, err := s.pool.Query(ctx, "SELECT copied")' TestOrderedIndexPlanGateUsesProductionStatements 'is not its receiver-bound pool consuming the one orderedquery.Ranked statement SQL and variadic Args'
run_mutation "ordered production receiver shadow" internal/orderedindex/orderedindex.go 'statement := orderedquery.Ranked(s.recordsTable(), namespace, rankingScope, queryPosition, limit)
	rows, err := s.pool.Query(ctx, statement.SQL, statement.Args...)' 'statement := orderedquery.Ranked(s.recordsTable(), namespace, rankingScope, queryPosition, limit)
	other := s
	var rows pgx.Rows
	{ s := other; rows, err = s.pool.Query(ctx, statement.SQL, statement.Args...) }' TestOrderedIndexPlanGateUsesProductionStatements 'the page statement must be executed here'
run_mutation "ordered plan gate dead helper live copied table" orderedindex_plan_integration_test.go 'for _, test := range orderedPlanCases(table, prefix) {' '_ = orderedPlanCases(table, prefix)
	tests := []orderedPlanCase{{name: "copied", family: orderedPlanOrder, page: orderedPlanFirst, indexName: prefix + "ordered_order_idx", statement: orderedquery.Statement{SQL: "SELECT 1"}, wantArgs: nil}}
	for _, test := range tests {' TestOrderedIndexPlanGateUsesProductionStatements 'has 0 direct orderedPlanCases ranges, want exactly 1'
run_integration_mutation "ordered plan gate same-name local shadow" orderedindex_plan_integration_test.go 'for _, test := range orderedPlanCases(table, prefix) {' 'deadOrderedPlanCases := orderedPlanCases
	_ = deadOrderedPlanCases
	orderedPlanCases := func(table, prefix string) []orderedPlanCase {
		return []orderedPlanCase{
			{name: "copied order first", family: orderedPlanOrder, page: orderedPlanFirst, indexName: prefix + "ordered_order_idx", statement: orderedquery.Statement{SQL: "SELECT 1"}},
			{name: "copied order middle", family: orderedPlanOrder, page: orderedPlanMiddle, indexName: prefix + "ordered_order_idx", statement: orderedquery.Statement{SQL: "SELECT 1"}},
			{name: "copied rank first", family: orderedPlanRanked, page: orderedPlanFirst, indexName: prefix + "ordered_rank_idx", statement: orderedquery.Statement{SQL: "SELECT 1"}},
			{name: "copied rank middle", family: orderedPlanRanked, page: orderedPlanMiddle, indexName: prefix + "ordered_rank_idx", statement: orderedquery.Statement{SQL: "SELECT 1"}},
			{name: "copied due first", family: orderedPlanDue, page: orderedPlanFirst, indexName: prefix + "ordered_due_idx", statement: orderedquery.Statement{SQL: "SELECT 1"}},
			{name: "copied due middle", family: orderedPlanDue, page: orderedPlanMiddle, indexName: prefix + "ordered_due_idx", statement: orderedquery.Statement{SQL: "SELECT 1"}},
		}
	}
	for _, test := range orderedPlanCases(table, prefix) {' TestOrderedIndexPlanGateUsesProductionStatements 'has 0 direct orderedPlanCases ranges, want exactly 1'
run_mutation "ordered plan gate typed nil marked middle" orderedindex_plan_integration_test.go '&orderedquery.RankedPosition{Rank: 50, StableKey: []byte("key-000500"), OrderingScope: "scope-5"}' '(*orderedquery.RankedPosition)(nil)' TestOrderedIndexPlanGateUsesProductionStatements 'page metadata orderedPlanMiddle disagrees with its orderedPlanFirst cursor'
run_integration_mutation "ordered get leaks bare password" internal/orderedindex/orderedindex.go 'return storage.OrderedRecord{}, failure(ctx, "ordered get")' 'return storage.OrderedRecord{}, pginternal.RedactedError("ordered get "+s.pool.Config().ConnConfig.Password)' TestOperationErrorsDoNotDiscloseDSNOrCredential 'OrderedIndex.Get error disclosed'
run_integration_mutation "ordered create leaks bare password" internal/orderedindex/orderedindex.go 'return storage.OrderedRecord{}, false, failure(ctx, "ordered create lookup")' 'return storage.OrderedRecord{}, false, pginternal.RedactedError("ordered create "+s.pool.Config().ConnConfig.Password)' TestOperationErrorsDoNotDiscloseDSNOrCredential 'OrderedIndex.Create error disclosed'
run_integration_mutation "ordered ranked list leaks bare password" internal/orderedindex/orderedindex.go 'return storage.RankedPage{}, failure(ctx, "ordered list ranked")' 'return storage.RankedPage{}, pginternal.RedactedError("ordered ranked "+s.pool.Config().ConnConfig.Password)' TestOperationErrorsDoNotDiscloseDSNOrCredential 'OrderedIndex.ListRanked error disclosed'
run_integration_mutation "ordered due list leaks bare password" internal/orderedindex/orderedindex.go 'return storage.DuePage{}, failure(ctx, "ordered list due")' 'return storage.DuePage{}, pginternal.RedactedError("ordered due "+s.pool.Config().ConnConfig.Password)' TestOperationErrorsDoNotDiscloseDSNOrCredential 'OrderedIndex.ListDue error disclosed'

# Seam invariant: an unimplemented operation may never return a nil or zero
# value without an error. The guard derives the unimplemented set, so the
# derivation is mutated here too, not just the operations.
run_mutation "implemented KV.Get claims NotImplemented" internal/kv/kv.go 'return nil, 0, &storage.KeyNotFoundError{Key: key}' 'return nil, 0, guard.NotImplemented("KV.Get")' TestSeamOperationsReturnNotImplemented 'calls guard.NotImplemented although KV conformance runs'
# The derivation must not be able to lose an input silently. Deleting the
# interface assertion compiles, because Open still assigns the concrete store to
# a storage.Leaser field, so nothing else notices; composed with a nil return it
# would reinstate the exact defect the guard exists to prevent.
run_mutation "seam derivation input deleted" internal/lease/lease.go 'var _ storage.Leaser = (*Store)(nil)' '' TestSeamOperationsReturnNotImplemented 'the seam derivation lost its input'
run_mutation "conformance entry point removed" conformance_integration_test.go 'func TestLeaserConformance' 'func LeaserConformanceRemoved' TestSeamOperationsReturnNotImplemented 'TestLeaserLifecycleConformance exists without TestLeaserConformance'
run_mutation "renewable lifecycle entry point removed" conformance_integration_test.go 'func TestLeaserLifecycleConformance' 'func LeaserLifecycleConformanceRemoved' TestSeamOperationsReturnNotImplemented 'renewable lifecycle conformance set ='

# Identifier budget: PostgreSQL truncates past 63 bytes silently.
run_mutation "oversized table suffix" internal/orderedindex/orderedindex.go 'var _ storage.OrderedIndex = (*Store)(nil)' 'var dueTable = func(tablePrefix string) string { return tablePrefix + "ordered_index_due_state_page" }

var _ storage.OrderedIndex = (*Store)(nil)' TestTableSuffixesFitTheReservedIdentifierBudget 'over the 23-byte reserve'
# Naming the suffix must not remove it from the budget.
run_mutation "oversized table suffix named as a constant" internal/orderedindex/orderedindex.go 'var _ storage.OrderedIndex = (*Store)(nil)' 'const duePageSuffix = "ordered_index_due_state_page"

func (s *Store) duePage() string { return s.tablePrefix + duePageSuffix }

var _ storage.OrderedIndex = (*Store)(nil)' TestTableSuffixesFitTheReservedIdentifierBudget 'over the 23-byte reserve'
# A suffix the derivation cannot read must be an error, not an omission.
run_mutation "unbounded table suffix operand" internal/orderedindex/orderedindex.go 'var _ storage.OrderedIndex = (*Store)(nil)' 'func (s *Store) computed(part string) string { return s.tablePrefix + part }

var _ storage.OrderedIndex = (*Store)(nil)' TestTableSuffixesFitTheReservedIdentifierBudget 'is not a string literal or a package-level string constant'

# Both halves of the Ledger append authoritative reread.
run_integration_mutation "absent record reported as success" internal/ledger/ledger.go 'return &storage.AmbiguousError{Name: name, Expected: expected, Cause: cause}' 'return nil' TestAppendReportsAmbiguousWhenTheRecordIsAbsentAfterAnUnknownCommit 'want *storage.AmbiguousError'
run_integration_mutation "another writer's record claimed as success" internal/ledger/ledger.go 'if err == nil && bytes.Equal(stored, payload) {' 'if err == nil {' TestAppendReportsConflictWhenAnotherWriterOwnsTheSequence 'want *storage.ConflictError'

# The retry classifier reached by an error PostgreSQL really raises, not an
# injected *pgconn.PgError.
run_integration_mutation "real serialization SQLSTATE classification" internal/postgres/postgres.go 'pgErr.Code == "40001"' 'pgErr.Code == "40002"' TestAppendRetriesARealSerializationFailureFromPostgreSQL 'want *storage.ConflictError from the retried attempt'

# Migration version policy.
run_integration_mutation "schema downgrade guard" migrations.go 'if version > currentSchemaVersion {' 'if false && version > currentSchemaVersion {' TestMigrationRefusesASchemaNewerThanThisBuild 'want the downgrade guard'
run_integration_mutation "validate accepts any version" migrations.go 'if version != currentSchemaVersion {' 'if false && version != currentSchemaVersion {' TestMigrationValidateDoesNotCreateAbsentSchema 'Open validate returned store for absent schema'
run_integration_mutation "already-applied migration replayed" migrations.go 'if parseErr != nil || n <= version {' 'if parseErr != nil || n < version {' TestMigrationIsIdempotentAndValidatesACurrentSchema 'second Open with MigrationApply on a current schema'

# Redaction, on every error path the implemented primitives construct rather
# than one representative path. Each mutation interpolates the live DSN.
run_integration_mutation "ledger append leaks DSN" internal/ledger/ledger.go 'return pginternal.RedactedError("ledger append")' "return errors.New(\"ledger append failed: ${PGSTORE_TEST_DSN}\")" TestOperationErrorsDoNotDiscloseDSNOrCredential 'Ledger.Append error disclosed'
run_integration_mutation "ledger read leaks DSN" internal/ledger/ledger.go 'return nil, operationFailure(ctx, "ledger read")' "return nil, errors.New(\"ledger read failed: ${PGSTORE_TEST_DSN}\")" TestOperationErrorsDoNotDiscloseDSNOrCredential 'Ledger.Read error disclosed'
run_integration_mutation "ledger tip leaks DSN" internal/ledger/ledger.go 'return 0, operationFailure(ctx, "ledger tip")' "return 0, errors.New(\"ledger tip failed: ${PGSTORE_TEST_DSN}\")" TestOperationErrorsDoNotDiscloseDSNOrCredential 'Ledger.Tip error disclosed'
run_integration_mutation "ledger delete resolution leaks DSN" internal/ledger/ledger.go 'return pginternal.RedactedError("ledger delete outcome resolution")' "return errors.New(\"ledger delete outcome resolution failed: ${PGSTORE_TEST_DSN}\")" TestOperationErrorsDoNotDiscloseDSNOrCredential 'Ledger.Delete error disclosed'
run_integration_mutation "kv get leaks DSN" internal/kv/kv.go 'return nil, 0, failure(ctx, "kv get")' "return nil, 0, pginternal.RedactedError(\"kv get on ${PGSTORE_TEST_DSN}\")" TestOperationErrorsDoNotDiscloseDSNOrCredential 'KV.Get error disclosed'
run_integration_mutation "lease acquire leaks DSN" internal/lease/lease.go 'return nil, leaseFailure(ctx, "lease acquire")' "return nil, pginternal.RedactedError(\"lease acquire on ${PGSTORE_TEST_DSN}\")" TestOperationErrorsDoNotDiscloseDSNOrCredential 'Leaser.Acquire error disclosed'
# D1: every adapter holding a pool can reach the parsed bare credentials even
# when the operation does not receive a DSN. These mutations prove the nonce
# credential guard catches both values through pgxpool.Config.
run_integration_mutation "D1 kv get leaks bare password" internal/kv/kv.go 'return nil, 0, failure(ctx, "kv get")' 'return nil, 0, pginternal.RedactedError("kv get "+s.pool.Config().ConnConfig.Password)' TestOperationErrorsDoNotDiscloseDSNOrCredential 'KV.Get error disclosed'
run_integration_mutation "D1 kv get leaks bare user" internal/kv/kv.go 'return nil, 0, failure(ctx, "kv get")' 'return nil, 0, pginternal.RedactedError("kv get "+s.pool.Config().ConnConfig.User)' TestOperationErrorsDoNotDiscloseDSNOrCredential 'KV.Get error disclosed'
run_integration_mutation "kv keys leaks DSN" internal/kv/kv.go 'return nil, failure(ctx, "kv keys")' "return nil, pginternal.RedactedError(\"kv keys on ${PGSTORE_TEST_DSN}\")" TestOperationErrorsDoNotDiscloseDSNOrCredential 'KV.Keys error disclosed'
run_integration_mutation "kv put resolution leaks DSN" internal/kv/kv.go 'return 0, pginternal.RedactedError("kv put outcome resolution")' "return 0, pginternal.RedactedError(\"kv put outcome resolution on ${PGSTORE_TEST_DSN}\")" TestOperationErrorsDoNotDiscloseDSNOrCredential 'KV.Put error disclosed'
run_integration_mutation "kv delete resolution leaks DSN" internal/kv/kv.go 'return pginternal.RedactedError("kv delete outcome resolution")' "return pginternal.RedactedError(\"kv delete outcome resolution on ${PGSTORE_TEST_DSN}\")" TestOperationErrorsDoNotDiscloseDSNOrCredential 'KV.Delete error disclosed'
run_integration_mutation "migration failure leaks DSN" migrations.go 'return pginternal.RedactedError("schema migration")' "return errors.New(\"schema migration failed: ${PGSTORE_TEST_DSN}\")" TestMigrationErrorsDoNotDiscloseDSNOrCredential 'error disclosed'

completed=1
