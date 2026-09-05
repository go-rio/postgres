# postgres

[![Doc](https://pkg.go.dev/badge/github.com/go-rio/postgres.svg)](https://pkg.go.dev/github.com/go-rio/postgres)
[![Go](https://img.shields.io/github/go-mod/go-version/go-rio/postgres)](https://go.dev/)
[![Release](https://img.shields.io/github/release/go-rio/postgres.svg)](https://github.com/go-rio/postgres/releases)
[![Test](https://github.com/go-rio/postgres/actions/workflows/test.yml/badge.svg)](https://github.com/go-rio/postgres/actions/workflows/test.yml)
[![License](https://img.shields.io/github/license/go-rio/postgres)](https://opensource.org/license/MIT)

PostgreSQL driver module for [rio](https://github.com/go-rio/rio), built on
[pgx](https://github.com/jackc/pgx): a database/sql channel (plain or over
pgxpool) and a native pgx channel with batched preloads and `COPY` inserts.
rio renders the SQL.

```go
db, err := postgres.OpenNative(ctx, "postgres://user:pass@localhost:5432/app")
if err != nil {
	return err
}
defer db.Close()

users, err := rio.From[User]().Where("age > ?", 18).With("Posts").All(ctx, db)
err = rio.InsertAll(ctx, db, rows) // explicit keys stream over COPY
```

## Getting started

```sh
go get github.com/go-rio/postgres
```

```go
package main

import (
	"context"
	"log"

	"github.com/go-rio/postgres"
	"github.com/go-rio/rio"
)

type User struct {
	ID    int64
	Email string
	Age   int
}

func main() {
	ctx := context.Background()
	db, err := postgres.Open("postgres://user:pass@localhost:5432/app")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	users, err := rio.From[User]().Where("age > ?", 18).All(ctx, db)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("%d adults", len(users))
}
```

Requires Go 1.27 and PostgreSQL 9.1 or later (`standard_conforming_strings`
on); the test suite runs against PostgreSQL 18.

## Features

### Execution paths

rio queries use the same API on all three paths.

| Path | Constructors | Use for |
|---|---|---|
| database/sql | `Open`, `New` | `*sql.DB` pooling, sqlmock, database/sql instrumentation. |
| database/sql over pgxpool | `OpenPool`, `NewFromPool` | pgxpool configuration and `PoolOf` with the database/sql query path. |
| native pgx | `OpenNative`, `NewNativeFromPool` | Lower scan allocations, one round trip per preload layer, `COPY` inserts, pgx execution modes and transaction semantics. |

`Open` validates the DSN — pgx URL or keyword/value form — but does not
connect; ping with `db.Unwrap().PingContext(ctx)`. `New` wraps an existing
`*sql.DB` and leaves session configuration to the caller.

### standard_conforming_strings

rio's placeholder lexer requires `standard_conforming_strings=on`, the
PostgreSQL default since 9.1. `Open`, `OpenPool`, and `OpenNative` reject an
explicit false — set directly, through the `options` startup parameter, or
through `PGOPTIONS` — and do not inject the setting when omitted. If the
server disables it globally, enable it in the DSN:

```text
postgres://user:pass@localhost:5432/app?options=-c%20standard_conforming_strings%3Don
```

`New`, `NewFromPool`, and `NewNativeFromPool` cannot inspect an existing
pool; their callers must enforce this requirement.

### pgxpool with database/sql

```go
db, err := postgres.OpenPool(ctx, "postgres://user:pass@localhost:5432/app?pool_max_conns=10")
if err != nil {
	log.Fatal(err)
}
defer db.Close()
```

- `OpenPool` accepts pgxpool `pool_*` DSN parameters; `NewFromPool` takes a
  caller-built pool. `PoolOf` exposes the pool for `Ping`, `Stat`,
  `AcquireFunc`, `CopyFrom`, and `LISTEN/NOTIFY`.
- Both constructors transfer pool ownership to the rio DB. Closing the DB
  closes the pool and may block until acquired connections return.
- Configure connection counts on pgxpool, not on `db.Unwrap()`; the
  database/sql view keeps no idle connections.

### Native pgx execution

`OpenNative` executes rio queries directly through pgx, preserving rio's SQL
rendering, scanning rules, sentinel errors, hooks, and savepoint behavior.
Preloads flush each relation layer in one pgx batch round trip. `InsertAll`
streams explicit-key batches — no generated keys to backfill — over `COPY`.

```go
db, err := postgres.OpenNative(ctx, "postgres://user:pass@localhost:5432/app")
```

`NewNativeFromPool` takes ownership of a caller-built pool; closing the DB
closes the pool and may block until acquired connections return.
`db.Unwrap()` stays available as a database/sql view for helpers such as
migrations and pings, never for pool configuration.

| Difference from database/sql | Detail |
|---|---|
| `tx.Unwrap()` returns nil | Use `postgres.TxOf(tx)` for the `pgx.Tx` (e.g. for `SET LOCAL`). Nested savepoints expose the root pgx transaction. |
| `rio.WithStmtCache` panics | Native statement caching belongs to pgx's query execution mode. |
| Error text may carry pgx prefixes | Check errors with `errors.Is` and `errors.As`. |

[rio's PostgreSQL benchmarks](https://github.com/go-rio/rio/blob/main/bench/bench_pg_test.go) compare both channels.

### Query exec mode and PgBouncer

`OpenNative` installs `AfterConnect`, which keeps `time`, `inet` and `cidr` in the
server's text form so string columns read the same as on the database/sql paths;
`NewNativeFromPool` callers add it to their `pgxpool.Config.AfterConnect`. Arrays
scan into a string only through database/sql; the native path asks for a slice.

The native path uses pgx's default `cache_statement` mode. Change it with the
`default_query_exec_mode` DSN parameter or on a caller-built pool config.

| Setup | Action |
|---|---|
| Direct connection | Keep the pgx default. |
| PgBouncer ≥ 1.21 with `max_prepared_statements > 0` | None. |
| Older PgBouncer in transaction/statement pooling | Add `default_query_exec_mode=exec` (or `simple_protocol`) to the DSN. Symptom otherwise: `prepared statement "stmtcache_..." does not exist`. |

On the database/sql paths, `rio.WithStmtCache` (off by default) adds DB- and
transaction-local caches on top of pgx's per-connection cache. Do not use it
behind transaction- or statement-mode poolers.

### Key sets and row locks

Preload and `WithCount` key sets bind as one typed array parameter
(`"posts"."user_id" = ANY($1)`) on both channels, so the statement text does
not vary with the number of parents and pgx's statement cache hits every
time; user `IN (?)` slices still expand. `ForUpdate` and `ForShare` accept
`rio.NoWait` and `rio.SkipLocked`, and `UpdateAllReturning` and
`DeleteAllReturning` scan the affected rows back.

### Arrays and JSONB

Tag JSON fields with `rio:",json"` to store them as `jsonb` (for example
`Prefs map[string]any` on an `Account` model, column `prefs`); a set-based
`rio.Set{"prefs": v}` uses the same mapping.

For PostgreSQL arrays, wrap pgtype in a `driver.Valuer` and `sql.Scanner`
pair. The wrapper works on all three paths:

```go
var pgMap = pgtype.NewMap()

type Tags []string

func (t Tags) Value() (driver.Value, error) {
	b, err := pgMap.Encode(pgtype.TextArrayOID, pgtype.TextFormatCode, pgtype.FlatArray[string](t), nil)
	if err != nil || b == nil {
		return nil, err
	}
	return string(b), nil
}

func (t *Tags) Scan(src any) error {
	return pgMap.SQLScanner((*pgtype.FlatArray[string])(t)).Scan(src)
}
```

For `UpdateAll`, pass the wrapper type or a `rio.Expr`, not a bare slice.

JSONB existence operators collide with rio's `?` placeholder. Escape each
literal question mark as `??` (`?|` → `??|`, `?&` → `??&`):

```go
rio.From[Account]().Where("prefs ?? ?", "beta").All(ctx, db) // prefs ? $1
```

### Error translation

| SQLSTATE | rio sentinel |
|---|---|
| 23505 | `rio.ErrDuplicateKey` |
| 23503 | `rio.ErrForeignKeyViolated` |

The original `*pgconn.PgError` stays available through `errors.As`, including
its constraint, table, and detail fields.

## Contributing

Read [CONTRIBUTING.md](CONTRIBUTING.md): a clone, `go test ./...`, and the
one-line Docker command for the gated integration tests.

## Contributors

Thanks to everyone who has filed issues and opened pull requests on
[go-rio/postgres](https://github.com/go-rio/postgres/graphs/contributors).

## License

The [MIT License](LICENSE). Copyright (c) 2026-now TreeNewBee.
