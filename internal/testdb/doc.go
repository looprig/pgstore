// Package testdb holds shared helpers for tests that need a real PostgreSQL
// database. It is currently empty.
//
// The runbook listed this package in pgstore's initial layout, and P1.1's doc
// comment said P1.2 would add a disposable-database fixture here. P1.2 did
// not: every integration test reads PGSTORE_TEST_DSN directly and skips when
// it is unset, provisioning its own uniquely prefixed schema objects rather
// than sharing a fixture. That is recorded here because the previous comment
// had become locally plausible and globally false. A later task may either
// consolidate that duplicated setup here or delete this package.
package testdb
