# Contributing to go-rio/postgres

## Prerequisites

- Go 1.27 or newer
- Docker, for the gated integration tests

## Setup

```sh
git clone https://github.com/go-rio/postgres
cd postgres
go build ./...
```

## Tests

```sh
go vet ./...
go test ./...
go test -race ./...
```

The integration tests cover all three channels and skip without
`RIO_POSTGRES_DSN`:

```sh
docker run -d --name rio-pg -e POSTGRES_PASSWORD=bench -p 127.0.0.1:15432:5432 postgres:18-alpine -c fsync=off
RIO_POSTGRES_DSN='postgres://postgres:bench@127.0.0.1:15432/postgres?sslmode=disable' go test -race ./...
docker rm -f rio-pg
```

## Pull requests

- Every change ships with a test; one test file per source file
  (`native.go` ↔ `native_test.go`, `pool.go` ↔ `pool_test.go`,
  `postgres.go` ↔ `postgres_test.go`).
- Comments state contracts. Exported identifiers get a doc comment naming
  purpose, constraints, and error cases; internal comments are one line, two
  at most; no history or narrative.
- Commit subjects carry a conventional prefix (`feat:`, `fix:`, `docs:`,
  `test:`, `chore:`).
- Keep `gofmt` and `go vet` clean; the native channel must not add
  allocations to the scan path.

## Releases

Maintainers tag signed releases (`git tag -s vX.Y.Z`) after the rio core
version they depend on is tagged, and record every user-visible change in
[CHANGELOG.md](CHANGELOG.md).
