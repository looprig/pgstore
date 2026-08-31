#!/bin/sh
set -eu

snapshot_dir="/private/tmp/pgstore-mutation-snapshot-${UID:-codex}"
files="go.mod options.go pgstore.go internal/guard/guard.go internal/ledger/ledger.go internal/lease/lease.go internal/kv/kv.go internal/orderedindex/orderedindex.go"

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
	GOWORK=off GOCACHE=/private/tmp/pgstore-gocache go test -list "^${test_name}$" . | grep -qx "$test_name"
	if ! grep -Fq "$old" "$file"; then
		echo "mutation pattern not found: $name"
		exit 1
	fi
	MUT_OLD=$old MUT_NEW=$new perl -0pi -e 's/\Q$ENV{"MUT_OLD"}\E/$ENV{"MUT_NEW"}/' "$file"

	set +e
	output=$(GOWORK=off GOCACHE=/private/tmp/pgstore-gocache go test -run "^${test_name}$" -count=1 . 2>&1)
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
run_mutation "context deadline" internal/guard/guard.go 'if _, ok := ctx.Deadline(); !ok {' 'if _, ok := ctx.Deadline(); ok && false {' TestOpenRequiresDeadline 'Open returned a Store without a caller deadline'
run_mutation "Open option short circuit" pgstore.go 'if err != nil {' 'if false && err != nil {' TestOpenRejectsOptionsBeforePoolConstruction 'want nil, error'
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

for operation in Ledger.Append Ledger.Read Ledger.Tip Ledger.Delete Leaser.Acquire KV.Get KV.Put KV.Keys KV.Delete OrderedIndex.Get OrderedIndex.Create OrderedIndex.Update OrderedIndex.Delete OrderedIndex.ListOrdered OrderedIndex.ListRanked OrderedIndex.ListDue; do
	case $operation in
		Ledger.*) file=internal/ledger/ledger.go ;;
		Leaser.*) file=internal/lease/lease.go ;;
		KV.*) file=internal/kv/kv.go ;;
		OrderedIndex.*) file=internal/orderedindex/orderedindex.go ;;
	esac
	run_mutation "$operation deadline call" "$file" "guard.RequireDeadline(ctx, \"$operation\")" "guard.NotImplemented(\"$operation\")" TestStructuredOperationMethodsCallDeadlineGuard 'does not call guard.RequireDeadline'
done

for operation in Ledger.Append Ledger.Read Ledger.Tip Ledger.Delete Leaser.Acquire KV.Get KV.Put KV.Keys KV.Delete OrderedIndex.Get OrderedIndex.Create OrderedIndex.Update OrderedIndex.Delete OrderedIndex.ListOrdered OrderedIndex.ListRanked OrderedIndex.ListDue; do
	case $operation in
		Ledger.*) file=internal/ledger/ledger.go ;;
		Leaser.*) file=internal/lease/lease.go ;;
		KV.*) file=internal/kv/kv.go ;;
		OrderedIndex.*) file=internal/orderedindex/orderedindex.go ;;
	esac
	run_mutation "$operation honest stub" "$file" "guard.NotImplemented(\"$operation\")" 'nil' TestStructuredOperationMethodsCallDeadlineGuard 'does not call guard.NotImplemented'
done

restore_snapshot
rm -rf "$snapshot_dir"
