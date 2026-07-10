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
pgx runtime parameter all work — with one exception, described in the next
section. `Open` validates the DSN but does not connect; ping `db.Unwrap()`
to check connectivity eagerly.

## standard_conforming_strings

rio rewrites `?` placeholders by lexing your SQL the way a stock PostgreSQL
server does: with `standard_conforming_strings=on` — the default since
PostgreSQL 9.1 — a backslash inside a `'...'` literal is an ordinary
character. Turning the setting off makes the server treat backslash as an
escape character again, so a literal in your SQL could hide or expose a `?`
differently on each side and the placeholder count would diverge (rio fails
loudly with an arity error rather than sending a misbound query). The
setting is therefore not supported. `Open` keeps the invariant the same way
the mysql sibling pins `sql_mode`:

- The setting is never mentioned → nothing is injected; the session uses
  the server's value, which is `on` unless an operator changed it.
- The DSN — or the `PGOPTIONS` environment variable, which pgx also reads —
  turns it off, either directly (`standard_conforming_strings=off`) or
  through the `options` startup parameter
  (`options=-c standard_conforming_strings=off`) → `Open` returns an error
  naming the setting.
- An explicit `on` is redundant but harmless and passes through.

If your server turns the setting off globally, turn it back on for rio's
connections in the DSN (URL form, `%20` is a space and `%3D` is `=`):

```text
postgres://user:pass@localhost:5432/app?options=-c%20standard_conforming_strings%3Don
```

or in keyword/value form:

```text
host=localhost dbname=app options='-c standard_conforming_strings=on'
```

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
never replaces or configures the connection pool. `New` performs no
connection hygiene either — keeping `standard_conforming_strings` on (see
above) is on you.

## PgBouncer

Behind PgBouncer in transaction or statement pooling mode, keep
`rio.WithStmtCache` off (it already is by default) and add
`default_query_exec_mode=exec` to the DSN, because server-side prepared
statements do not survive connection multiplexing.

Talking to PostgreSQL directly, leave `rio.WithStmtCache` off too: pgx
already caches prepared statements per connection in its default query exec
mode, and stacking `database/sql`'s statement layer on top measured slower,
not faster, in rio's bench suite.

## The rio family

[rio](https://github.com/go-rio/rio) — the ORM ·
[migrate](https://github.com/go-rio/migrate) — schema migrations as Go code ·
[sqlite](https://github.com/go-rio/sqlite) / [mysql](https://github.com/go-rio/mysql) —
the sibling drivers

## License

The MIT License (MIT). Please see [License File](LICENSE) for more
information.
