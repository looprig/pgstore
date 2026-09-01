package pgstore

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const testDSN = "postgres://looprig:super-secret@db.example.test:5432/looprig?sslmode=verify-full"

func TestOptionsResolve(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		mutate     func(*Options)
		wantField  string
		wantReason string
	}{
		{name: "valid defaults"},
		{name: "missing DSN", mutate: func(o *Options) { o.DSN = "" }, wantField: "DSN", wantReason: "must be set"},
		{name: "whitespace DSN", mutate: func(o *Options) { o.DSN = "  \t" }, wantField: "DSN", wantReason: "must be set"},
		{name: "malformed DSN", mutate: func(o *Options) { o.DSN = "://not-a-dsn" }, wantField: "DSN", wantReason: "invalid PostgreSQL connection string"},
		{name: "insecure DSN", mutate: func(o *Options) { o.DSN = "postgres://db.example.test/looprig?sslmode=disable" }, wantField: "DSN", wantReason: "must require verified TLS"},
		{name: "TLS fallback DSN", mutate: func(o *Options) { o.DSN = "postgres://db.example.test/looprig?sslmode=prefer" }, wantField: "DSN", wantReason: "must require verified TLS"},
		{name: "unverified TLS DSN", mutate: func(o *Options) { o.DSN = "postgres://db.example.test/looprig?sslmode=require" }, wantField: "DSN", wantReason: "must require verified TLS"},
		{name: "minimum connections negative", mutate: func(o *Options) { o.MinConns = -1 }, wantField: "MinConns", wantReason: "must not be negative"},
		{name: "minimum exceeds maximum", mutate: func(o *Options) { o.MinConns = 3; o.MaxConns = 2 }, wantField: "MinConns", wantReason: "must not exceed MaxConns"},
		{name: "maximum connections negative", mutate: func(o *Options) { o.MaxConns = -1 }, wantField: "MaxConns", wantReason: "must be positive"},
		{name: "invalid schema", mutate: func(o *Options) { o.Schema = `tenant"; DROP SCHEMA public;--` }, wantField: "Schema", wantReason: "lowercase PostgreSQL identifier"},
		{name: "uppercase schema", mutate: func(o *Options) { o.Schema = "Sessions" }, wantField: "Schema", wantReason: "lowercase PostgreSQL identifier"},
		{name: "oversized schema", mutate: func(o *Options) { o.Schema = strings.Repeat("s", 64) }, wantField: "Schema", wantReason: "at most 63 bytes"},
		{name: "invalid table prefix", mutate: func(o *Options) { o.TablePrefix = "../../sessions" }, wantField: "TablePrefix", wantReason: "lowercase identifier prefix"},
		{name: "uppercase table prefix", mutate: func(o *Options) { o.TablePrefix = "Sessions_" }, wantField: "TablePrefix", wantReason: "lowercase identifier prefix"},
		{name: "oversized table prefix", mutate: func(o *Options) { o.TablePrefix = strings.Repeat("p", 41) }, wantField: "TablePrefix", wantReason: "at most 40 bytes"},
		{name: "negative statement timeout", mutate: func(o *Options) { o.StatementTimeout = -time.Second }, wantField: "StatementTimeout", wantReason: "must be positive"},
		{name: "sub-millisecond statement timeout", mutate: func(o *Options) { o.StatementTimeout = time.Nanosecond }, wantField: "StatementTimeout", wantReason: "at least one millisecond"},
		{name: "negative lock timeout", mutate: func(o *Options) { o.LockTimeout = -time.Second }, wantField: "LockTimeout", wantReason: "must be positive"},
		{name: "sub-millisecond lock timeout", mutate: func(o *Options) { o.LockTimeout = time.Nanosecond }, wantField: "LockTimeout", wantReason: "at least one millisecond"},
		{name: "lock timeout exceeds statement timeout", mutate: func(o *Options) { o.LockTimeout = 2 * time.Minute; o.StatementTimeout = time.Minute }, wantField: "LockTimeout", wantReason: "must not exceed StatementTimeout"},
		{name: "negative lease TTL", mutate: func(o *Options) { o.LeaseTTL = -time.Second }, wantField: "LeaseTTL", wantReason: "must be positive"},
		{name: "sub-millisecond lease TTL", mutate: func(o *Options) { o.LeaseTTL = time.Nanosecond }, wantField: "LeaseTTL", wantReason: "at least one millisecond"},
		{name: "negative lease renew interval", mutate: func(o *Options) { o.LeaseRenewInterval = -time.Second }, wantField: "LeaseRenewInterval", wantReason: "must be positive"},
		{name: "sub-millisecond lease renew interval", mutate: func(o *Options) { o.LeaseRenewInterval = time.Nanosecond }, wantField: "LeaseRenewInterval", wantReason: "at least one millisecond"},
		{name: "lease renew interval equals TTL", mutate: func(o *Options) { o.LeaseTTL = time.Second; o.LeaseRenewInterval = time.Second }, wantField: "LeaseRenewInterval", wantReason: "must be shorter than LeaseTTL"},
		{name: "lease renew interval exceeds TTL", mutate: func(o *Options) { o.LeaseTTL = time.Second; o.LeaseRenewInterval = 2 * time.Second }, wantField: "LeaseRenewInterval", wantReason: "must be shorter than LeaseTTL"},
		{name: "invalid migration mode", mutate: func(o *Options) { o.Migrations = MigrationMode(99) }, wantField: "Migrations", wantReason: "unknown mode"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := Options{DSN: testDSN}
			if tt.mutate != nil {
				tt.mutate(&opts)
			}
			got, err := opts.resolve()
			if tt.wantField == "" {
				if err != nil {
					t.Fatalf("resolve: %v", err)
				}
				if got.maxConns != defaultMaxConns || got.schema != defaultSchema || got.tablePrefix != defaultTablePrefix {
					t.Fatalf("resolved defaults = max %d schema %q prefix %q", got.maxConns, got.schema, got.tablePrefix)
				}
				if got.statementTimeout != defaultStatementTimeout || got.lockTimeout != defaultLockTimeout {
					t.Fatalf("resolved timeouts = statement %s lock %s", got.statementTimeout, got.lockTimeout)
				}
				if got.leaseTTL != defaultLeaseTTL || got.leaseRenewInterval != defaultLeaseRenewInterval || got.leaseRenewInterval >= got.leaseTTL {
					t.Fatalf("resolved lease timing = TTL %s renew %s", got.leaseTTL, got.leaseRenewInterval)
				}
				return
			}
			var optionsErr *OptionsError
			if !errors.As(err, &optionsErr) {
				t.Fatalf("resolve error = %T %v, want *OptionsError", err, err)
			}
			if optionsErr.Field != tt.wantField {
				t.Errorf("OptionsError.Field = %q, want %q", optionsErr.Field, tt.wantField)
			}
			if !strings.Contains(optionsErr.Reason, tt.wantReason) {
				t.Errorf("OptionsError.Reason = %q, want substring %q", optionsErr.Reason, tt.wantReason)
			}
			if strings.Contains(err.Error(), "super-secret") || (opts.DSN != "" && strings.Contains(err.Error(), opts.DSN)) {
				t.Fatalf("error disclosed DSN or credential: %q", err)
			}
		})
	}
}

func TestOptionsResolveAppliesValidCustomValues(t *testing.T) {
	t.Parallel()

	resolved, err := (Options{
		DSN:                testDSN,
		MinConns:           2,
		MaxConns:           4,
		Schema:             strings.Repeat("s", 63),
		TablePrefix:        strings.Repeat("p", 39) + "_",
		StatementTimeout:   2 * time.Second,
		LockTimeout:        time.Second,
		LeaseTTL:           9 * time.Second,
		LeaseRenewInterval: 3 * time.Second,
		Migrations:         MigrationApply,
	}).resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.minConns != 2 || resolved.maxConns != 4 || resolved.poolConfig.MinConns != 2 || resolved.poolConfig.MaxConns != 4 {
		t.Fatalf("resolved pool bounds = (%d, %d), config = (%d, %d)", resolved.minConns, resolved.maxConns, resolved.poolConfig.MinConns, resolved.poolConfig.MaxConns)
	}
	if _, ok := resolved.poolConfig.ConnConfig.RuntimeParams["statement_timeout"]; ok {
		t.Error("statement_timeout is a startup RuntimeParam, incompatible with transaction pooling")
	}
	if _, ok := resolved.poolConfig.ConnConfig.RuntimeParams["lock_timeout"]; ok {
		t.Error("lock_timeout is a startup RuntimeParam, incompatible with transaction pooling")
	}
	if resolved.poolConfig.ConnConfig.DefaultQueryExecMode != pgx.QueryExecModeExec {
		t.Errorf("query exec mode = %v, want QueryExecModeExec without a connection-local statement cache", resolved.poolConfig.ConnConfig.DefaultQueryExecMode)
	}
	if resolved.migrations != MigrationApply {
		t.Errorf("migrations = %v, want MigrationApply", resolved.migrations)
	}
	if resolved.leaseTTL != 9*time.Second || resolved.leaseRenewInterval != 3*time.Second {
		t.Errorf("lease timing = TTL %s renew %s, want 9s and 3s", resolved.leaseTTL, resolved.leaseRenewInterval)
	}
}

func TestOptionsResolveAllowsExplicitLocalInsecureTransport(t *testing.T) {
	t.Parallel()

	resolved, err := (Options{
		DSN:                        "postgres://localhost/looprig?sslmode=disable",
		AllowInsecureLocalhostOnly: true,
	}).resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.poolConfig.ConnConfig.TLSConfig != nil {
		t.Fatal("TLSConfig is non-nil for explicit local insecure transport")
	}
}

func TestOptionsResolveRejectsUnverifiedLocalTLSDespiteTestOptIn(t *testing.T) {
	t.Parallel()

	_, err := (Options{
		DSN:                        "postgres://localhost/looprig?sslmode=require",
		AllowInsecureLocalhostOnly: true,
	}).resolve()
	var optionsErr *OptionsError
	if !errors.As(err, &optionsErr) || !strings.Contains(optionsErr.Reason, "verified TLS") {
		t.Fatalf("resolve error = %T %v, want verified-TLS *OptionsError", err, err)
	}
}

func TestOptionsResolveRejectsExplicitRemoteInsecureTransport(t *testing.T) {
	t.Parallel()

	_, err := (Options{
		DSN:                        "postgres://db.example.test/looprig?sslmode=disable",
		AllowInsecureLocalhostOnly: true,
	}).resolve()
	var optionsErr *OptionsError
	if !errors.As(err, &optionsErr) || optionsErr.Field != "DSN" || !strings.Contains(optionsErr.Reason, "loopback") {
		t.Fatalf("resolve error = %T %v, want loopback-only *OptionsError", err, err)
	}
}

func TestOptionsResolveAllowsLoopbackIPInsecureTransport(t *testing.T) {
	t.Parallel()

	_, err := (Options{
		DSN:                        "postgres://127.0.0.1/looprig?sslmode=disable",
		AllowInsecureLocalhostOnly: true,
	}).resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
}

func TestOptionsErrorNeverUnwrapsDSNParserError(t *testing.T) {
	t.Parallel()

	_, err := (Options{DSN: "postgres://user:super-secret@%"}).resolve()
	var optionsErr *OptionsError
	if !errors.As(err, &optionsErr) {
		t.Fatalf("resolve error = %T %v, want *OptionsError", err, err)
	}
	if errors.Unwrap(err) != nil {
		t.Fatalf("OptionsError unwrap = %v, want nil", errors.Unwrap(err))
	}
	if strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("error disclosed credential: %q", err)
	}
}
