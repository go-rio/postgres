# postgres

[![Doc](https://pkg.go.dev/badge/github.com/go-rio/postgres)](https://pkg.go.dev/github.com/go-rio/postgres)
[![Go](https://img.shields.io/github/go-mod/go-version/go-rio/postgres)](https://go.dev/)
[![Release](https://img.shields.io/github/release/go-rio/postgres.svg)](https://github.com/go-rio/postgres/releases)
[![Test](https://github.com/go-rio/postgres/actions/workflows/test.yml/badge.svg)](https://github.com/go-rio/postgres/actions)
[![Report Card](https://goreportcard.com/badge/github.com/go-rio/postgres)](https://goreportcard.com/report/github.com/go-rio/postgres)
[![License](https://img.shields.io/github/license/go-rio/postgres)](https://opensource.org/license/MIT)

PostgreSQL driver module for [rio](https://github.com/go-rio/rio), the
zero-surprise Go ORM, built on [pgx](https://github.com/jackc/pgx) — through
its database/sql adapter by default, or fully natively (`OpenNative`) for the
fastest read path.

The module is deliberately thin — that is the design, not a first draft. It
provides the constructors, eager DSN validation, a precise error translator
that maps `*pgconn.PgError` onto rio's sentinels (SQLSTATE 23505 becomes
`rio.ErrDuplicateKey`, 23503 becomes `rio.ErrForeignKeyViolated`, the
original pgx error kept in the chain for `errors.As`), and the pgx-native
execution channel. Everything that shapes SQL lives in the rio core.

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

## Choosing a constructor

Three tiers, each with a bring-your-own variant; DAO code is identical on
all of them, and moving between tiers is a one-line constructor swap.

| Tier | Constructors | What you get |
|---|---|---|
| database/sql | `Open` · `New` | The default. database/sql manages connections, so everything in that ecosystem — `sqlmock`, otelsql wrappers, your own `*sql.DB` tuning — plugs in unchanged. |
| pgx pool | `OpenPool` · `NewFromPool` | Same query semantics, pgxpool manages connections: health checks, connection lifetime and idle caps, `AfterConnect`, `Stat()` metrics — and `PoolOf(db)` opens the door to `CopyFrom` and `LISTEN`. Measured performance-neutral next to `Open`; choose it for the pool, not for speed. |
| pgx native | `OpenNative` · `NewNativeFromPool` | The fastest channel: queries run on pgx directly, no `driver.Value` boxing. Same SQL, same scanning rules, same errors, same hooks and savepoints — measured on loopback (median of 3): the 100-row read drops from 433 to 124 allocs/op (−71%, −20% bytes), single-row reads 30→18, Insert 19→14, Update 9→6. pgx semantics apply: exec mode comes from the DSN, and `TxOf(tx)` replaces `tx.Unwrap()` inside transactions. |

## The pgx pool tier

`OpenPool` parses the DSN with `pgxpool.ParseConfig` — every `Open` DSN
works, plus pgxpool's `pool_*` parameters (`pool_max_conns`,
`pool_min_conns`, `pool_max_conn_lifetime`, `pool_max_conn_idle_time`,
`pool_health_check_period`) — and applies the same
`standard_conforming_strings` guard:

```go
db, err := postgres.OpenPool(ctx, "postgres://user:pass@localhost:5432/app?pool_max_conns=10")
if err != nil {
	log.Fatal(err)
}
defer db.Close() // closes the database/sql view, then the pool

pool := postgres.PoolOf(db)    // *pgxpool.Pool: Ping, Stat, CopyFrom, LISTEN
err = pool.Ping(ctx)           // OpenPool validates but never connects
```

`NewFromPool` wraps a pool you built from your own `pgxpool.Config`
(tracers, `AfterConnect`, `MinConns`, a custom query exec mode). Like `New`
taking over the `*sql.DB`, both constructors take over the pool's `Close`:
closing the rio handle closes the pool, blocking until acquired connections
are returned; a further `pool.Close()` of your own is a harmless no-op. Keep
a pool out of `NewFromPool` if it must outlive the rio.DB.

Connection counts belong to the pgxpool configuration on this tier — leave
`SetMaxOpenConns`/`SetMaxIdleConns` off `db.Unwrap()`. The view keeps zero
idle database/sql connections (pgx's documented requirement), so an idle
view connection never pins a pool connection away from direct pool users.

## The native tier

`OpenNative` builds the same pgxpool as `OpenPool` and then skips the
database/sql layer entirely: rio's rendered SQL goes straight to pgx, and
decoded values flow through pgtype's typed scanner interfaces into rio's
scan cells with no boxing in between.

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

Everything rio promises holds unchanged — same rendered SQL, scanning rules
(NULL handling, overflow checks, `[]byte` copying), sentinel errors,
`QueryHook` events, savepoint choreography, `errors.Is(err,
context.Canceled)` on cancellation. The full integration suite runs twice in
CI, once per channel, to keep it that way. Three differences, all loud:

- **`tx.Unwrap()` returns nil** inside transactions — there is no `*sql.Tx`
  on this tier. Use `postgres.TxOf(tx)` for the `pgx.Tx`. (`db.Unwrap()`
  still works: it returns a database/sql view over the same pool for
  pool-agnostic helpers like pings and migrations; don't tune pooling on it.)
- **`rio.WithStmtCache` panics at construction** — statement caching belongs
  to pgx's query exec mode here (see below), not to a `database/sql` layer
  that no longer exists.
- **Error text can carry pgx prefixes** (timeouts, scan errors). The
  `errors.Is`/`errors.As` contracts are identical — only prose differs.

Numbers (Apple M4, loopback PostgreSQL 17, `bench/bench_pg_test.go`, median
of 3; real networks shrink the latency share but the allocation savings are
CPU-side and stay):

| shape | rio (stdlib) | rio (native) | hand-written database/sql | GORM |
|---|---|---|---|---|
| read 1 row | 30 allocs · 1.3 KB | **18 allocs · 1.0 KB** | 30 allocs · 1.3 KB | 82 allocs · 6.5 KB |
| read 100 rows | 433 allocs · 33 KB | **124 allocs · 27 KB** | 532 allocs · 41 KB | 1172 allocs · 59 KB |
| insert | 19 allocs | **14 allocs** | 20 allocs | 93 allocs |
| update | 9 allocs | **6 allocs** | 7 allocs | 93 allocs |

For comparison, pgx's own `pgx.CollectRows[T]` idiom costs ~316 allocs on
the 100-row shape — the native channel is faster than the driver's own
collection helper, not just faster than database/sql.

## Query exec mode and PgBouncer

The native tier uses pgx's own default execution mode,
`QueryExecModeCacheStatement`: statements are prepared and cached per
connection automatically. rio does not downgrade it behind your back —
choosing `OpenNative` is choosing pgx, and its default is part of the deal.
Change it in the DSN (`?default_query_exec_mode=exec`, `simple_protocol`,
`cache_describe`, …) or on your own `pgxpool.Config` via `NewNativeFromPool`.

| Your setup | What to do |
|---|---|
| Direct connection | Nothing. The default (`cache_statement`) is the fast path. |
| PgBouncer ≥ 1.21 with `max_prepared_statements > 0` | Nothing. PgBouncer tracks prepared statements across the multiplexer; the default works. |
| Older PgBouncer in transaction/statement pooling | Add `default_query_exec_mode=exec` (or `simple_protocol`) to the DSN. Symptom if you don't: errors like `prepared statement "stmtcache_..." does not exist`. |

DDL note: under `cache_statement`, changing a table's shape invalidates
cached plans; pgx detects `cached plan must not change result type`,
invalidates, and retries read queries by itself. (On the database/sql tiers
the same situation is handled by rio's `WithStmtCache` eviction — which
evicts and propagates, never retries; both behaviors are documented, they
are just each layer's own.)

On the database/sql tiers behind PgBouncer, keep `rio.WithStmtCache` off (it
already is by default) and apply the same DSN matrix. Talking to PostgreSQL
directly, leave `rio.WithStmtCache` off there too: pgx already caches
prepared statements per connection, and stacking `database/sql`'s statement
layer on top measured slower, not faster, in rio's bench suite.

## The rio family

[rio](https://github.com/go-rio/rio) — the ORM ·
[migrate](https://github.com/go-rio/migrate) — schema migrations as Go code ·
[sqlite](https://github.com/go-rio/sqlite) / [mysql](https://github.com/go-rio/mysql) —
the sibling drivers

## License

The MIT License (MIT). Please see [License File](LICENSE) for more
information.
