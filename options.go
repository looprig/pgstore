package pgstore

import (
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	defaultMaxConns            int32         = 10
	defaultSchema                            = "looprig"
	defaultTablePrefix                       = "looprig_"
	defaultStatementTimeout    time.Duration = 30 * time.Second
	defaultLockTimeout         time.Duration = 5 * time.Second
	maxPostgresIdentifierBytes               = 63
	// maxTableSuffixBytes reserves identifier budget for the longest suffix a
	// primitive may append to TablePrefix ("schema_migrations" is 17 today).
	// PostgreSQL truncates an identifier past 63 bytes silently rather than
	// failing, so an over-long prefix would not error: it would map two
	// distinct tables onto one name. A primitive added by P1.3 or P1.4 whose
	// suffix exceeds this reserve must raise it, which lowers the accepted
	// TablePrefix length. TestTableSuffixesFitTheReservedIdentifierBudget
	// derives the actual suffixes from production source and enforces this.
	maxTableSuffixBytes = 23
	maxTablePrefixBytes = maxPostgresIdentifierBytes - maxTableSuffixBytes
)

// MigrationMode controls what Open may do with embedded schema migrations.
// P1.1 validates and retains this policy; P1.2 implements the migration runner.
type MigrationMode uint8

const (
	// MigrationValidate requires an already-current schema and never changes it.
	MigrationValidate MigrationMode = iota
	// MigrationApply permits Open to take the migration lock and apply upgrades.
	MigrationApply
	// MigrationDisabled bypasses migration checks for externally managed schemas.
	MigrationDisabled
)

// Options configures a PostgreSQL structured-primitives Store.
type Options struct {
	// DSN is required. It may contain credentials and is never copied into an error.
	DSN string

	MinConns int32
	MaxConns int32

	Schema      string
	TablePrefix string

	StatementTimeout time.Duration
	LockTimeout      time.Duration

	Migrations MigrationMode

	// AllowInsecureLocalhostOnly permits sslmode=disable only when pgx resolves
	// the target as a loopback host. It exists for disposable local test databases.
	AllowInsecureLocalhostOnly bool
}

// OptionsError reports one invalid option without retaining its value or parser
// cause. In particular, errors for DSN never disclose or unwrap credentials.
type OptionsError struct {
	Field  string
	Reason string
}

func (e *OptionsError) Error() string {
	return "pgstore: invalid option " + strconv.Quote(e.Field) + ": " + e.Reason
}

type resolvedOptions struct {
	poolConfig       *pgxpool.Config
	minConns         int32
	maxConns         int32
	schema           string
	tablePrefix      string
	statementTimeout time.Duration
	lockTimeout      time.Duration
	migrations       MigrationMode
}

func (o Options) resolve() (resolvedOptions, error) {
	if strings.TrimSpace(o.DSN) == "" {
		return resolvedOptions{}, invalidOption("DSN", "must be set")
	}
	poolConfig, err := pgxpool.ParseConfig(o.DSN)
	if err != nil {
		return resolvedOptions{}, invalidOption("DSN", "invalid PostgreSQL connection string")
	}
	if err := validateTLS(poolConfig, o.AllowInsecureLocalhostOnly); err != nil {
		return resolvedOptions{}, err
	}

	maxConns := o.MaxConns
	if maxConns < 0 {
		return resolvedOptions{}, invalidOption("MaxConns", "must be positive or zero for the default")
	}
	if maxConns == 0 {
		maxConns = defaultMaxConns
	}
	if o.MinConns < 0 {
		return resolvedOptions{}, invalidOption("MinConns", "must not be negative")
	}
	if o.MinConns > maxConns {
		return resolvedOptions{}, invalidOption("MinConns", "must not exceed MaxConns")
	}

	schema := o.Schema
	if schema == "" {
		schema = defaultSchema
	}
	if !validIdentifier(schema, maxPostgresIdentifierBytes) {
		return resolvedOptions{}, invalidOption("Schema", "must be a lowercase PostgreSQL identifier of at most 63 bytes")
	}
	tablePrefix := o.TablePrefix
	if tablePrefix == "" {
		tablePrefix = defaultTablePrefix
	}
	if !validIdentifier(tablePrefix, maxTablePrefixBytes) {
		return resolvedOptions{}, invalidOption("TablePrefix", "must be a lowercase identifier prefix of at most "+strconv.Itoa(maxTablePrefixBytes)+" bytes, leaving room for the longest table suffix within PostgreSQL's 63-byte identifier limit")
	}

	statementTimeout, err := resolveTimeout("StatementTimeout", o.StatementTimeout, defaultStatementTimeout)
	if err != nil {
		return resolvedOptions{}, err
	}
	lockTimeout, err := resolveTimeout("LockTimeout", o.LockTimeout, defaultLockTimeout)
	if err != nil {
		return resolvedOptions{}, err
	}
	if lockTimeout > statementTimeout {
		return resolvedOptions{}, invalidOption("LockTimeout", "must not exceed StatementTimeout")
	}
	if o.Migrations > MigrationDisabled {
		return resolvedOptions{}, invalidOption("Migrations", "has an unknown mode")
	}

	poolConfig.MinConns = o.MinConns
	poolConfig.MaxConns = maxConns
	// FORWARD CONSTRAINT (P1.3 step 1, P1.5 step 2): these are startup
	// RuntimeParams, sent in the connection handshake, and pgx's default exec
	// mode caches prepared statements per connection. Both are incompatible
	// with PgBouncer in transaction pooling mode, which the lease topology and
	// the released deployment documentation are required to support. Whoever
	// takes that requirement must move these to per-transaction SET LOCAL (or
	// an AfterConnect hook) and select a non-caching QueryExecMode; do not
	// simply document the limitation away.
	poolConfig.ConnConfig.RuntimeParams["statement_timeout"] = strconv.FormatInt(statementTimeout.Milliseconds(), 10)
	poolConfig.ConnConfig.RuntimeParams["lock_timeout"] = strconv.FormatInt(lockTimeout.Milliseconds(), 10)

	return resolvedOptions{
		poolConfig: poolConfig, minConns: o.MinConns, maxConns: maxConns,
		schema: schema, tablePrefix: tablePrefix,
		statementTimeout: statementTimeout, lockTimeout: lockTimeout,
		migrations: o.Migrations,
	}, nil
}

func invalidOption(field, reason string) *OptionsError {
	return &OptionsError{Field: field, Reason: reason}
}

func validateTLS(config *pgxpool.Config, allowInsecureLocal bool) error {
	secure := config.ConnConfig.TLSConfig != nil && !config.ConnConfig.TLSConfig.InsecureSkipVerify
	if secure {
		return nil
	}
	if allowInsecureLocal && isLoopbackHost(config.ConnConfig.Host) && config.ConnConfig.TLSConfig == nil {
		return nil
	}
	return invalidOption("DSN", "must require verified TLS; plaintext is allowed only for an explicitly enabled loopback test database")
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func validIdentifier(value string, maxBytes int) bool {
	if value == "" || len(value) > maxBytes || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for i := 1; i < len(value); i++ {
		char := value[i]
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '_' {
			return false
		}
	}
	return true
}

func resolveTimeout(field string, requested, fallback time.Duration) (time.Duration, error) {
	if requested < 0 {
		return 0, invalidOption(field, "must be positive or zero for the default")
	}
	if requested == 0 {
		return fallback, nil
	}
	if requested < time.Millisecond {
		return 0, invalidOption(field, "must be at least one millisecond")
	}
	return requested, nil
}
