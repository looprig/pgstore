# Transactional Epoch Leases Design

## Goal

Implement PostgreSQL-backed renewable epoch leases that remain correct through
ordinary pools and transaction-pooling proxies. No operation depends on a
physical connection or PostgreSQL session, and no lease operation uses an
advisory lock.

## Public configuration

`Options` gains `LeaseTTL` and `LeaseRenewInterval`. Zero selects documented
defaults. Both values must be at least one millisecond, and the renewal
interval must be strictly shorter than the TTL. The resolved values are passed
to the lease adapter without adding a dependency.

## Persistent model

A second monotonic migration adds one lease table. Each validated lease name is
the primary key and retains:

- the greatest epoch ever granted;
- the current random holder token, or no holder after release;
- the current database expiry;
- a monotonically increasing revision.

Rows are persistent. Release and cleanup clear ownership but never delete a row
or reset its epoch. Holder tokens come from `crypto/rand` and are stored as
opaque bytes.

## Acquire

Acquire validates its deadline and name, starts a transaction, creates an empty
row when necessary, and locks the row with `SELECT ... FOR UPDATE`. An expiry
strictly greater than PostgreSQL's `clock_timestamp()` is held; equality is
expired. A free or expired row advances epoch and revision, installs a fresh
holder token and database-computed expiry, and commits.

The insert-plus-row-lock transaction serializes first acquisition as well as
reacquisition. Commit acknowledgement loss is resolved by an authoritative
reread for the exact epoch and holder token. An observed exact committed grant
returns the lease; a conflicting grant returns held/lost state; an unreadable
outcome remains a redacted ambiguous failure.

## Renewal and loss

Every acquired lease owns one renewal goroutine. Each renewal is a standalone
pool operation and may use a different connection. It updates only a row whose
name, epoch, holder token, and unexpired state all match, advances revision,
and receives the new database expiry.

An ambiguous renewal is unsafe until an authoritative reread proves that the
same epoch/token is still current and unexpired. A successful proof resumes
renewal. Failure to prove ownership closes `Lost()`. A definite failed compare,
expiry, database outage past the safe renewal window, or observation of a later
epoch also closes it. Closure is guarded by `sync.Once`.

The renewal loop uses the database-returned expiry and a conservative local
deadline so it cannot assert ownership beyond the last expiry it successfully
observed. Store shutdown cancels all renewal activity and closes every live
lease's `Lost()` channel.

## Release

Release requires a deadline and is idempotent. It first ends the local grant
and closes `Lost()`, then conditionally clears holder and expiry using name,
epoch, and holder token. A stale release cannot affect a later holder.
Ambiguous release uses an authoritative reread; already-cleared or superseded
state is successful because the requested grant no longer owns the row.

## Error and security rules

All SQL values are parameters. Only validated, quoted schema/table identifiers
are interpolated. Driver errors, DSNs, users, and passwords never escape. Lease
operation failures use redacted errors or Storage's typed lease errors. The
whole production package remains free of lease/operation advisory locks; the
existing transaction-scoped migration lock is the single counted exemption.

## Verification

Test-first integration rounds cover acquisition, contention, renewal, exact
expiry boundaries, release, increasing epochs, `Lost()`, cancellation,
connection turnover, restart/outage, ambiguous acknowledgements, stale writers,
and later-epoch observation. Source-derived guards prove the advisory-lock
boundary and operation coverage. Storage v0.6.0's `TestLeaser` and
`TestLeaserLifecycle` run alongside provider-specific tests. Mutations must
fail named, pre-listed tests with behavioral—not compile/setup—failures.
