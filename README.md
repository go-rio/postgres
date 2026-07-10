# postgres

[![Doc](https://pkg.go.dev/badge/github.com/go-rio/postgres)](https://pkg.go.dev/github.com/go-rio/postgres)
[![Go](https://img.shields.io/github/go-mod/go-version/go-rio/postgres)](https://go.dev/)
[![Release](https://img.shields.io/github/release/go-rio/postgres.svg)](https://github.com/go-rio/postgres/releases)
[![Test](https://github.com/go-rio/postgres/actions/workflows/test.yml/badge.svg)](https://github.com/go-rio/postgres/actions)
[![Report Card](https://goreportcard.com/badge/github.com/go-rio/postgres)](https://goreportcard.com/report/github.com/go-rio/postgres)
[![License](https://img.shields.io/github/license/go-rio/postgres)](https://opensource.org/license/MIT)

PostgreSQL driver module for [rio](https://github.com/go-rio/rio), the Go ORM,
built on [pgx](https://github.com/jackc/pgx). Runs through the pgx database/sql
adapter by default, or fully natively (`OpenNative`) for the fastest read path.

It provides constructors, eager DSN validation, the pgx-native execution
channel, and an error translator that maps `*pgconn.PgError` onto rio
sentinels, keeping the original pgx error in the chain for `errors.As`. SQL
rendering stays in the rio core.

| SQLSTATE | rio sentinel |
|---|---|
| 23505 | `rio.ErrDuplicateKey` |
| 23503 | `rio.ErrForeignKeyViolated` |

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
	errors.As(err, &pgErr) // constraint name, table, detail preserved
}
```

- The DSN is passed to pgx untouched: URL form, keyword/value form, and every
  pgx runtime parameter, except `standard_conforming_strings=off` (below).
- `Open` validates the DSN without connecting; ping `db.Unwrap()` to check
  connectivity eagerly.

## standard_conforming_strings

rio rewrites `?` placeholders assuming `standard_conforming_strings=on` (the
default since PostgreSQL 9.1), where backslash inside a `'...'` literal is
ordinary. Turned off, backslash becomes an escape character, so rio and the
server can disagree on the placeholder count; rio fails with an arity error,
not a misbound query. The setting is unsupported, as the mysql sibling pins
`sql_mode`.

| DSN state | Result |
|---|---|
| Not mentioned | Nothing injected; the session uses the server value (`on` unless an operator changed it). |
| `on`, explicit | Redundant but harmless; passes through. |
| Off — directly (`standard_conforming_strings=off`), via the `options` startup parameter (`options=-c standard_conforming_strings=off`), or via `PGOPTIONS` (pgx also reads it) | `Open` returns an error naming the setting. |

Re-enable per connection when the server disables it globally (URL form,
`%20` is a space, `%3D` is `=`):

```text
postgres://user:pass@localhost:5432/app?options=-c%20standard_conforming_strings%3Don
```

Keyword/value form:

```text
host=localhost dbname=app options='-c standard_conforming_strings=on'
```

## Choosing a constructor

Three tiers, each with a bring-your-own variant. DAO code is identical across
tiers; switching is a one-line constructor swap.

| Tier | Constructors | Notes |
|---|---|---|
| database/sql | `Open` · `New` | Default. database/sql manages connections, so `sqlmock`, otelsql wrappers, and `*sql.DB` tuning plug in unchanged. |
| pgx pool | `OpenPool` · `NewFromPool` | Same query semantics; pgxpool manages connections (health checks, connection lifetime and idle caps, `AfterConnect`, `Stat()` metrics). `PoolOf(db)` exposes `CopyFrom` and `LISTEN`. Measured performance-neutral next to `Open`. |
| pgx native | `OpenNative` · `NewNativeFromPool` | Fastest channel: queries run on pgx directly, no `driver.Value` boxing. Same SQL, scanning rules, errors, hooks, and savepoints. Loopback median-of-3: 100-row read 433→124 allocs/op (−71%, −20% bytes), single-row 30→18, Insert 19→14, Update 9→6. pgx semantics apply: exec mode comes from the DSN, and `TxOf(tx)` replaces `tx.Unwrap()` in transactions. |

## The pgx pool tier

`OpenPool` parses the DSN with `pgxpool.ParseConfig`: every `Open` DSN plus
pgxpool `pool_*` parameters (`pool_max_conns`, `pool_min_conns`,
`pool_max_conn_lifetime`, `pool_max_conn_idle_time`,
`pool_health_check_period`). The `standard_conforming_strings` guard still
applies.

```go
db, err := postgres.OpenPool(ctx, "postgres://user:pass@localhost:5432/app?pool_max_conns=10")
if err != nil {
	log.Fatal(err)
}
defer db.Close() // closes the database/sql view, then the pool

pool := postgres.PoolOf(db)    // *pgxpool.Pool: Ping, Stat, CopyFrom, LISTEN
err = pool.Ping(ctx)           // OpenPool validates but never connects
```

- `NewFromPool` wraps a pool built from your own `pgxpool.Config` (tracers,
  `AfterConnect`, `MinConns`, a custom query exec mode).
- Both constructors take over the pool's `Close` (as `New` does for `*sql.DB`):
  closing the rio handle closes the pool, blocking until acquired connections
  return; a later `pool.Close()` is a no-op. Keep a pool out of `NewFromPool`
  if it must outlive the rio.DB.
- Connection counts belong to the pgxpool config here; leave
  `SetMaxOpenConns`/`SetMaxIdleConns` off `db.Unwrap()`. The view holds zero
  idle database/sql connections (pgx's documented requirement), so an idle
  view connection never pins a pool connection away from direct pool users.

## The native tier

`OpenNative` builds the same pgxpool as `OpenPool`, then skips database/sql:
rendered SQL goes straight to pgx, and decoded values flow through pgtype's
typed scanner interfaces into rio's scan cells with no boxing.

```go
db, err := postgres.OpenNative(ctx, "postgres://user:pass@localhost:5432/app")
if err != nil {
	log.Fatal(err)
}
defer db.Close() // closes the database/sql view, then the pool

pool := postgres.PoolOf(db)      // the pgxpool: Ping, Stat, CopyFrom, LISTEN
err = db.Tx(ctx, func(tx *rio.Tx) error {
	ptx := postgres.TxOf(tx)     // the pgx.Tx behind this transaction
	_ = ptx                      // CopyFrom inside the transaction, etc.
	return nil
})
```

Contracts hold as on the other tiers: same rendered SQL, scanning rules (NULL
handling, overflow checks, `[]byte` copying), sentinel errors, `QueryHook`
events, savepoint choreography, and `errors.Is(err, context.Canceled)` on
cancellation. The full integration suite runs twice in CI, once per channel.
Three differences:

| Difference | Detail |
|---|---|
| `tx.Unwrap()` returns nil in transactions | No `*sql.Tx` exists here; use `postgres.TxOf(tx)` for the `pgx.Tx`. `db.Unwrap()` still returns a database/sql view over the same pool for pool-agnostic helpers (pings, migrations); do not tune pooling on it. |
| `rio.WithStmtCache` panics at construction | Statement caching belongs to pgx's query exec mode here, not to an absent database/sql layer. |
| Error text can carry pgx prefixes | Affects timeouts and scan errors. `errors.Is`/`errors.As` contracts are identical; only prose differs. |

Benchmarks (Apple M4, loopback PostgreSQL 17, `bench/bench_pg_test.go`, median
of 3; real networks shrink the latency share, but the allocation savings are
CPU-side and stay):

| shape | rio (stdlib) | rio (native) | hand-written database/sql | GORM |
|---|---|---|---|---|
| read 1 row | 30 allocs · 1.3 KB | **18 allocs · 1.0 KB** | 30 allocs · 1.3 KB | 82 allocs · 6.5 KB |
| read 100 rows | 433 allocs · 33 KB | **124 allocs · 27 KB** | 532 allocs · 41 KB | 1172 allocs · 59 KB |
| insert | 19 allocs | **14 allocs** | 20 allocs | 93 allocs |
| update | 9 allocs | **6 allocs** | 7 allocs | 93 allocs |

pgx's own `pgx.CollectRows[T]` costs ~316 allocs on the 100-row shape — the
native channel beats even that helper.

## Query exec mode and PgBouncer

The native tier uses pgx's default execution mode,
`QueryExecModeCacheStatement`: statements are prepared and cached per
connection automatically. rio never downgrades it. Change it in the DSN
(`?default_query_exec_mode=exec`, `simple_protocol`, `cache_describe`, …) or on
your own `pgxpool.Config` via `NewNativeFromPool`.

| Setup | Action |
|---|---|
| Direct connection | None; the default (`cache_statement`) is the fast path. |
| PgBouncer ≥ 1.21 with `max_prepared_statements > 0` | None; PgBouncer tracks prepared statements across the multiplexer. |
| Older PgBouncer in transaction/statement pooling | Add `default_query_exec_mode=exec` (or `simple_protocol`) to the DSN. Symptom otherwise: `prepared statement "stmtcache_..." does not exist`. |

DDL note: under `cache_statement`, changing a table's shape invalidates cached
plans; pgx detects `cached plan must not change result type`, invalidates, and
retries read queries itself. On the database/sql tiers, rio's `WithStmtCache`
eviction handles the same case — it evicts and propagates, never retries.

On the database/sql tiers behind PgBouncer, keep `rio.WithStmtCache` off (the
default) and apply the same DSN matrix. Against PostgreSQL directly, leave it
off too: pgx already caches prepared statements per connection, and stacking
database/sql's statement layer on top measured slower in rio's bench suite.

## The rio family

[rio](https://github.com/go-rio/rio) — the ORM ·
[migrate](https://github.com/go-rio/migrate) — schema migrations as Go code ·
[sqlite](https://github.com/go-rio/sqlite) / [mysql](https://github.com/go-rio/mysql) —
the sibling drivers

## License

The MIT License (MIT). Please see [License File](LICENSE) for more
information.
