# postgres

[![Doc](https://pkg.go.dev/badge/github.com/go-rio/postgres)](https://pkg.go.dev/github.com/go-rio/postgres)
[![Go](https://img.shields.io/github/go-mod/go-version/go-rio/postgres)](https://go.dev/)
[![Release](https://img.shields.io/github/release/go-rio/postgres.svg)](https://github.com/go-rio/postgres/releases)
[![Test](https://github.com/go-rio/postgres/actions/workflows/test.yml/badge.svg)](https://github.com/go-rio/postgres/actions)
[![Report Card](https://goreportcard.com/badge/github.com/go-rio/postgres)](https://goreportcard.com/report/github.com/go-rio/postgres)
[![License](https://img.shields.io/github/license/go-rio/postgres)](https://opensource.org/license/MIT)

PostgreSQL driver module for [rio](https://github.com/go-rio/rio), the
zero-surprise Go ORM, built on [pgx](https://github.com/jackc/pgx)'s
database/sql adapter.

The module is deliberately thin — that is the design, not a first draft. It
provides the constructors, eager DSN validation, and a precise error
translator that maps `*pgconn.PgError` onto rio's sentinels: SQLSTATE 23505
becomes `rio.ErrDuplicateKey` and 23503 becomes `rio.ErrForeignKeyViolated`,
with the original pgx error kept in the chain for `errors.As`. Everything
that shapes SQL lives in the rio core.

## Install

```sh
go get github.com/go-rio/postgres
```

## Usage

```go
db, err := postgres.Open("postgres://user:pass@localhost:5432/app")
if err != nil {
	log.Fatal(err)
}
defer db.Close()

err = rio.Insert(ctx, db, &user) // RETURNING fills the whole row back

if errors.Is(err, rio.ErrDuplicateKey) {
	var pgErr *pgconn.PgError
	errors.As(err, &pgErr) // constraint name, table, detail — all still there
}
```

The DSN is handed to pgx untouched: URL form, keyword/value form, and every
pgx runtime parameter all work. `Open` validates the DSN but does not
connect; ping `db.Unwrap()` to check connectivity eagerly.

## Bring your own pool

`New` wraps any `*sql.DB` you already manage — including one derived from a
`pgxpool.Pool`:

```go
pool, err := pgxpool.New(ctx, dsn)
if err != nil {
	log.Fatal(err)
}
db := postgres.New(stdlib.OpenDBFromPool(pool))
```

Pool tuning (`SetMaxOpenConns` and friends) happens on the `*sql.DB`; rio
never replaces or configures the connection pool.

## PgBouncer

Behind PgBouncer in transaction or statement pooling mode, keep
`rio.WithStmtCache` off (it already is by default) and add
`default_query_exec_mode=exec` to the DSN, because server-side prepared
statements do not survive connection multiplexing.

## The rio family

[rio](https://github.com/go-rio/rio) — the ORM ·
[migrate](https://github.com/go-rio/migrate) — schema migrations as Go code ·
[sqlite](https://github.com/go-rio/sqlite) / [mysql](https://github.com/go-rio/mysql) —
the sibling drivers

## License

The MIT License (MIT). Please see [License File](LICENSE) for more
information.
