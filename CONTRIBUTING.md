# Contributing to looprig/pgstore

Thanks for contributing. Read [`CLAUDE.md`](CLAUDE.md) before changing code; it
defines the dependency, credential-handling, SQL, and testing boundaries.

## Before writing code

- Keep this provider limited to Ledger, Leaser, KV, and OrderedIndex. Blobs
  belong to `s3store`, and SessionStore composition belongs to consumers.
- Do not add a local `replace`, vendor dependencies, or add an ORM/migration
  framework. The approved direct dependencies are Storage and pgx v5.
- Write a failing test first and run it. Database behavior belongs in an
  `integration`-tagged test against a disposable PostgreSQL instance.
- Never put a DSN or credential into test failure output, errors, or logs.

## Checks

Every target runs the module standalone with `GOWORK=off`.

```sh
make fmt
make fmt-check
make vet
make test
make test-integration
make check
make secure
```

`make test-integration` skips the P1.1 conformance wiring unless
`PGSTORE_TEST_DSN` is set. P1.2 will provide and own disposable database setup.

## Pull requests

Keep changes repository-local and focused. Include the red/green commands,
mutation evidence for new guards, and standalone verification output. Do not
commit secrets, force-push reviewed work, or mix release/tag work into a feature
commit.

Contributions are licensed under Apache License 2.0; see [`LICENSE`](LICENSE).
