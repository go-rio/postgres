# postgres

[![Doc](https://pkg.go.dev/badge/github.com/go-rio/postgres.svg)](https://pkg.go.dev/github.com/go-rio/postgres)
[![Go](https://img.shields.io/github/go-mod/go-version/go-rio/postgres)](https://go.dev/)
[![Release](https://img.shields.io/github/release/go-rio/postgres.svg)](https://github.com/go-rio/postgres/releases)
[![Test](https://github.com/go-rio/postgres/actions/workflows/test.yml/badge.svg)](https://github.com/go-rio/postgres/actions/workflows/test.yml)
[![License](https://img.shields.io/github/license/go-rio/postgres)](https://opensource.org/license/MIT)

PostgreSQL driver module for [rio](https://github.com/go-rio/rio), built on
[pgx](https://github.com/jackc/pgx). It supports database/sql, database/sql over
pgxpool, and direct pgx execution.

## Getting started

```sh
go get github.com/go-rio/postgres
```

The default path uses `database/sql`:

```go
db, err := postgres.Open("postgres://user:pass@localhost:5432/app")
if err != nil {
	log.Fatal(err)
}
defer db.Close()

if err := db.Unwrap().PingContext(ctx); err != nil {
	log.Fatal(err)
}

if err := rio.Insert(ctx, db, &user); err != nil {
	log.Fatal(err)
}
```

`Open` validates the DSN but does not connect. It accepts pgx URL and
keyword/value DSNs. Use `New` to wrap an existing `*sql.DB`; the caller remains
responsible for its session configuration.

### Error translation

| SQLSTATE | rio sentinel |
|---|---|
| 23505 | `rio.ErrDuplicateKey` |
| 23503 | `rio.ErrForeignKeyViolated` |

The original `*pgconn.PgError` remains available through `errors.As`, including
its constraint, table, and detail fields.

## Choosing an execution path

rio queries use the same API on all three paths. Choose based on connection
ownership and access to pgx APIs.

| Path | Constructors | Use when |
|---|---|---|
| database/sql | `Open`, `New` | You want `*sql.DB` pooling, sqlmock, or database/sql instrumentation. |
| database/sql over pgxpool | `OpenPool`, `NewFromPool` | You want pgxpool configuration and `PoolOf`, while keeping the database/sql query path. |
| native pgx | `OpenNative`, `NewNativeFromPool` | You want lower scan allocations and can use pgx execution-mode and transaction semantics. |

## standard_conforming_strings

rio's PostgreSQL placeholder lexer requires `standard_conforming_strings=on`,
the PostgreSQL default since 9.1. `Open`, `OpenPool`, and `OpenNative` reject an
explicit false value supplied directly, through the `options` startup
parameter, or through `PGOPTIONS`.

If omitted, rio does not inject the setting. Servers that disable it globally
must enable it for rio connections:

```text
postgres://user:pass@localhost:5432/app?options=-c%20standard_conforming_strings%3Don
```

Keyword/value DSNs can use
`options='-c standard_conforming_strings=on'` instead.

The bring-your-own constructors cannot validate an existing pool's session
settings; callers of `New`, `NewFromPool`, and `NewNativeFromPool` must enforce
this requirement.

## pgxpool with database/sql

```go
db, err := postgres.OpenPool(ctx, "postgres://user:pass@localhost:5432/app?pool_max_conns=10")
if err != nil {
	log.Fatal(err)
}
defer db.Close()

pool := postgres.PoolOf(db)
if err := pool.Ping(ctx); err != nil {
	log.Fatal(err)
}
```

- `OpenPool` accepts pgxpool `pool_*` DSN parameters. Use `NewFromPool` for a
  caller-built `pgxpool.Config`.
- `PoolOf` exposes the pool for `Ping`, `Stat`, `AcquireFunc`, `CopyFrom`, and
  `LISTEN/NOTIFY`.
- `OpenPool` and `NewFromPool` transfer pool ownership to the rio DB. Closing
  the DB closes the pool and may block until acquired connections return. Do
  not transfer a pool that must outlive the DB.
- Configure connection counts on pgxpool, not on `db.Unwrap()`. The
  database/sql view intentionally keeps no idle connections.

## Native pgx execution

`OpenNative` executes rio queries directly through pgx. `db.Unwrap()` remains
available as a database/sql view for helpers such as migrations and pings, but
must not be used to configure pooling.

```go
db, err := postgres.OpenNative(ctx, "postgres://user:pass@localhost:5432/app")
if err != nil {
	log.Fatal(err)
}
defer db.Close()

pool := postgres.PoolOf(db)
if err := pool.Ping(ctx); err != nil {
	log.Fatal(err)
}
err = db.Tx(ctx, func(tx *rio.Tx) error {
	ptx := postgres.TxOf(tx)
	_, err := ptx.Exec(ctx, "SET LOCAL lock_timeout = '1s'")
	return err
})
```

`NewNativeFromPool` accepts a caller-built pool and transfers ownership to the
rio DB. Closing the DB closes its database/sql view first and then the pool,
and may block until acquired connections return.

The native path preserves rio's SQL rendering, scanning rules, sentinel
errors, hooks, and savepoint behavior. Its public differences are:

| Difference | Detail |
|---|---|
| `tx.Unwrap()` returns nil | Use `postgres.TxOf(tx)` for the `pgx.Tx`. Nested rio savepoints expose the root pgx transaction. |
| `rio.WithStmtCache` panics | Native statement caching belongs to pgx's query execution mode. |
| Error text may carry pgx prefixes | Use `errors.Is` and `errors.As` for stable checks. |

The native path removes the `database/sql` conversion layer. Use
[rio's PostgreSQL benchmarks](https://github.com/go-rio/rio/blob/main/bench/bench_pg_test.go)
to compare both paths against the application workload.

## Query exec mode and PgBouncer

The native path uses pgx's default `cache_statement` mode. Change it with the
`default_query_exec_mode` DSN parameter or on a caller-built pool config.

| Setup | Action |
|---|---|
| Direct connection | Keep the pgx default. |
| PgBouncer ≥ 1.21 with `max_prepared_statements > 0` | None; PgBouncer tracks prepared statements across the multiplexer. |
| Older PgBouncer in transaction/statement pooling | Add `default_query_exec_mode=exec` (or `simple_protocol`) to the DSN. Symptom otherwise: `prepared statement "stmtcache_..." does not exist`. |

On database/sql paths, `rio.WithStmtCache` adds a DB-level cache and a cache
local to each transaction. It is off by default and is unsuitable for
transaction/statement-mode poolers. pgx already caches statements per
connection, so enable the additional rio cache only after measuring it.

## Arrays and JSONB

Tag JSON fields with `rio:",json"`:

```go
type Account struct {
	ID    int64
	Prefs map[string]any `rio:",json"` // jsonb column "prefs"
}
```

A set-based `rio.Set{"prefs": v}` uses the same JSON mapping.

For PostgreSQL arrays, define a `driver.Valuer` and `sql.Scanner` wrapper around
pgtype:

```go
var pgMap = pgtype.NewMap()

type Tags []string

func (t Tags) Value() (driver.Value, error) {
	b, err := pgMap.Encode(
		pgtype.TextArrayOID,
		pgtype.TextFormatCode,
		pgtype.FlatArray[string](t),
		nil,
	)
	if err != nil || b == nil {
		return nil, err
	}
	return string(b), nil
}

func (t *Tags) Scan(src any) error {
	return pgMap.SQLScanner((*pgtype.FlatArray[string])(t)).Scan(src)
}
```

The wrapper works on all three execution paths.

JSONB existence operators collide with rio's `?` placeholder. Escape each
literal question mark as `??`:

```go
rio.From[Account]().Where("prefs ?? ?", "beta").All(ctx, db)
```

This renders `prefs ? $1`; write `?|` and `?&` as `??|` and `??&`.

For `UpdateAll`, wrap an array slice in the `Valuer` type rather than passing a
bare slice, or use `rio.Expr` for a database-side expression.

## Contributing

Use Go 1.27 or newer, then run `go test ./...`, `go test -race ./...`, and
`go vet ./...` before opening a pull request.

## License

[MIT](LICENSE)
