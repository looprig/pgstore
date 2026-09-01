#!/bin/sh
set -eu

snapshot_dir="/private/tmp/pgstore-mutation-snapshot-${UID:-codex}"
files="go.mod options.go pgstore.go migrations.go internal/guard/guard.go internal/postgres/postgres.go internal/ledger/ledger.go internal/lease/lease.go internal/kv/kv.go internal/orderedindex/orderedindex.go"

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

run_mutation() {
	name=$1
	file=$2
	old=$3
	new=$4
	test_name=$5
	want=$6

	# The previous mutation is restored at the next iteration's entry. If this
	# process is killed mid-test, the startup recovery above performs this step.
	restore_snapshot
	GOWORK=off GOCACHE=/private/tmp/pgstore-gocache go test -list "^${test_name}$" ./... | grep -qx "$test_name"
	if ! grep -Fq "$old" "$file"; then
		echo "mutation pattern not found: $name"
		exit 1
	fi
	MUT_OLD=$old MUT_NEW=$new perl -0pi -e 's/\Q$ENV{"MUT_OLD"}\E/$ENV{"MUT_NEW"}/' "$file"

	set +e
	output=$(GOWORK=off GOCACHE=/private/tmp/pgstore-gocache go test -run "^${test_name}$" -count=1 ./... 2>&1)
	status=$?
	set -e
	if test "$status" -eq 0; then
		echo "SURVIVED|$name|$test_name|test passed"
		exit 1
	fi
	if printf '%s\n' "$output" | grep -Eq '\[build failed\]|\[setup failed\]|undefined:|syntax error'; then
		echo "INVALID|$name|$test_name|compile/setup failure"
		printf '%s\n' "$output"
		exit 1
	fi
	if ! printf '%s\n' "$output" | grep -Fq "$want"; then
		echo "WRONG_FAILURE|$name|$test_name|missing: $want"
		printf '%s\n' "$output"
		exit 1
	fi
	echo "KILLED|$name|$test_name|$want"
}

run_integration_mutation() {
	name=$1
	file=$2
	old=$3
	new=$4
	test_name=$5
	want=$6

	restore_snapshot
	PGSTORE_TEST_DSN=${PGSTORE_TEST_DSN:?PGSTORE_TEST_DSN is required for P1.2 mutations}
	export PGSTORE_TEST_DSN
	GOWORK=off GOCACHE=/private/tmp/pgstore-gocache go test -tags integration -list "^${test_name}$" ./... | grep -qx "$test_name"
	if ! grep -Fq "$old" "$file"; then
		echo "mutation pattern not found: $name"
		exit 1
	fi
	MUT_OLD=$old MUT_NEW=$new perl -0pi -e 's/\Q$ENV{"MUT_OLD"}\E/$ENV{"MUT_NEW"}/' "$file"

	set +e
	output=$(GOWORK=off GOCACHE=/private/tmp/pgstore-gocache go test -tags integration -run "^${test_name}$" -count=1 ./... 2>&1)
	status=$?
	set -e
	if test "$status" -eq 0; then
		echo "SURVIVED|$name|$test_name|test passed"
		exit 1
	fi
	if printf '%s\n' "$output" | grep -Eq '\[build failed\]|\[setup failed\]|undefined:|syntax error'; then
		echo "INVALID|$name|$test_name|compile/setup failure"
		printf '%s\n' "$output"
		exit 1
	fi
	if ! printf '%s\n' "$output" | grep -Fq "$want"; then
		echo "WRONG_FAILURE|$name|$test_name|missing: $want"
		printf '%s\n' "$output"
		exit 1
	fi
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
run_mutation "migration enum" options.go 'if o.Migrations > MigrationDisabled {' 'if false && o.Migrations > MigrationDisabled {' TestOptionsResolve 'invalid_migration_mode'
run_mutation "nil context" internal/guard/guard.go 'if ctx == nil {' 'if false && ctx == nil {' TestOperationRejectsNilContext 'panic:'
run_mutation "context deadline" internal/guard/guard.go 'if _, ok := ctx.Deadline(); !ok {' 'if _, ok := ctx.Deadline(); ok && false {' TestOpenRequiresDeadline 'want *DeadlineRequiredError'
run_mutation "Open option short circuit" pgstore.go 'if err != nil {' 'if false && err != nil {' TestOpenRejectsOptionsBeforePoolConstruction 'panic:'
run_mutation "Open deadline" pgstore.go 'guard.RequireDeadline(ctx, "Open")' 'guard.NotImplemented("Open")' TestOpenRequiresDeadline 'want *DeadlineRequiredError'
run_mutation "pool error redaction" pgstore.go 'invalidOption("DSN", "PostgreSQL pool configuration was rejected")' 'invalidOption("DSN", "PostgreSQL pool configuration was rejected: "+err.Error())' TestOpenRedactsPoolConstructionError 'want non-unwrapping redacted error'
run_mutation "nil Close" pgstore.go 'if s == nil {' 'if false && s == nil {' TestStoreCloseIsNilSafeAndIdempotent 'panic:'
run_mutation "idempotent Close" pgstore.go 's.closeOnce.Do(s.closePool)' 's.closePool()' TestStoreCloseIsNilSafeAndIdempotent 'pool close calls = 2'
run_mutation "local replace directive" go.mod 'go 1.26.6' 'go 1.26.6

replace github.com/looprig/storage => ../storage' TestDependencyBoundary 'replace directives, want none'
run_mutation "extra direct module" go.mod 'github.com/jackc/puddle/v2 v2.2.2 // indirect' 'github.com/jackc/puddle/v2 v2.2.2' TestDependencyBoundary 'direct modules ='
run_mutation "logging import" pgstore.go '"context"' '"context"
	_ "log/slog"' TestDependencyBoundary 'imports logging package "log/slog"'
run_mutation "Blobs field" pgstore.go 'Ledger       storage.Ledger' 'Blobs        storage.Blobs
	Ledger       storage.Ledger' TestOpenWiresStructuredPrimitivesWithoutBlobs 'Store exposes a Blobs field'

for operation in Leaser.Acquire OrderedIndex.Get OrderedIndex.Create OrderedIndex.Update OrderedIndex.Delete OrderedIndex.ListOrdered OrderedIndex.ListRanked OrderedIndex.ListDue; do
	case $operation in
		Ledger.*) file=internal/ledger/ledger.go ;;
		Leaser.*) file=internal/lease/lease.go ;;
		KV.*) file=internal/kv/kv.go ;;
		OrderedIndex.*) file=internal/orderedindex/orderedindex.go ;;
	esac
	run_mutation "$operation deadline call" "$file" "guard.RequireDeadline(ctx, \"$operation\")" "guard.NotImplemented(\"$operation\")" TestStructuredOperationMethodsCallDeadlineGuard 'does not call guard.RequireDeadline'
done

run_mutation "retry SQLSTATE classification" internal/postgres/postgres.go 'pgErr.Code == "40001"' 'pgErr.Code == "40002"' TestRetryableClassifiesOnlySerializationAndDeadlockSQLStates 'Retryable(SQLSTATE 40001) = false, want true'
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

restore_snapshot
rm -rf "$snapshot_dir"
