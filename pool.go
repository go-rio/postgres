package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"runtime"
	"sync"
	"weak"

	"github.com/go-rio/rio"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
)

// OpenPool builds a pgxpool.Pool from the DSN and wraps it in a *rio.DB whose
// queries run through pgx's database/sql adapter over that pool. Query
// semantics are exactly Open's — same SQL, same scanning, same errors; what
// changes is who manages connections. pgxpool brings health checks,
// connection lifetime and idle caps, the AfterConnect hook, and Stat()
// metrics; measured against Open the read path is performance-neutral, so
// the reason to choose OpenPool is pool semantics, not speed.
//
// The DSN accepts everything Open's does plus pgxpool's pool_* parameters
// (pool_max_conns, pool_min_conns, pool_max_conn_lifetime,
// pool_max_conn_idle_time, pool_health_check_period). A configuration that
// turns standard_conforming_strings off is rejected exactly as in Open. Like
// Open, OpenPool validates eagerly but does not connect — pgxpool connects
// lazily; use PoolOf(db).Ping(ctx) to verify connectivity.
//
// PoolOf returns the pool behind the *rio.DB: pool statistics, Ping, and
// pgx-native abilities such as CopyFrom and LISTEN all go through it. For a
// pool built from your own pgxpool.Config (tracers, AfterConnect, a custom
// query exec mode), use NewFromPool.
//
// Closing: db.Close() closes both the database/sql view and the pool,
// blocking until acquired connections are returned (pgxpool.Close
// semantics); closing the pool again via PoolOf is a harmless no-op. If you
// close the pool first instead, the view's queries fail with pgxpool's
// "closed pool" error, and db.Close() remains safe. Do not call
// SetMaxOpenConns or SetMaxIdleConns on db.Unwrap() — connections belong to
// the pgxpool configuration, and the view deliberately keeps zero idle
// database/sql connections (pgx's documented requirement, so an idle view
// connection never pins a pool connection and starves direct pool users).
func OpenPool(ctx context.Context, dsn string, opts ...rio.Option) (*rio.DB, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: open pool: %w", err)
	}
	if bad := nonConformingStringsSetting(cfg.ConnConfig.RuntimeParams); bad != "" {
		return nil, errNonConformingStrings("open pool", bad)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: open pool: %w", err)
	}
	return NewFromPool(pool, opts...), nil
}

// NewFromPool wraps a caller-built pgxpool.Pool in a *rio.DB, for pools that
// need a custom pgxpool.Config — tracers, AfterConnect hooks, MinConns, or a
// non-default query exec mode on the ConnConfig. Everything else matches
// OpenPool, including the closing contract: like New taking over the
// *sql.DB's Close, closing the rio.DB closes the pool you passed in
// (blocking until acquired connections are returned; your own pool.Close()
// afterwards is a harmless no-op). Keep the pool out of NewFromPool if it
// must outlive the rio.DB.
//
// NewFromPool performs no connection hygiene — the pool is the caller's;
// make sure its sessions run with standard_conforming_strings on (the server
// default since PostgreSQL 9.1), or rio's placeholder rewriting can disagree
// with the server's lexing (see Open).
func NewFromPool(pool *pgxpool.Pool, opts ...rio.Option) *rio.DB {
	if pool == nil {
		panic("postgres: NewFromPool: pool must not be nil")
	}
	view := sql.OpenDB(poolConnector{Connector: stdlib.GetPoolConnector(pool), pool: pool})
	// Zero idle view connections — pgx's documented requirement for a
	// database/sql view over pgxpool (stdlib.OpenDBFromPool sets the same):
	// an idle *sql.DB connection holds an acquired pool connection and would
	// starve direct pool users.
	view.SetMaxIdleConns(0)
	db := New(view, opts...)
	registerPool(db, pool)
	return db
}

// PoolOf returns the pgxpool.Pool behind a *rio.DB built by OpenPool,
// NewFromPool, OpenNative, or NewNativeFromPool, and nil for every other
// construction (Open and New manage no pool). It is the door to what the
// pool alone can do: Ping, Stat, AcquireFunc, CopyFrom, LISTEN/NOTIFY.
func PoolOf(db *rio.DB) *pgxpool.Pool {
	if db == nil {
		return nil
	}
	// The native channel carries its pool as the DB's native handle; the
	// pgx-pool-over-database/sql tier predates that slot and keeps its
	// package-local registry.
	if pool, ok := db.Native().(*pgxpool.Pool); ok {
		return pool
	}
	pools.RLock()
	defer pools.RUnlock()
	return pools.m[weak.Make(db)]
}

// poolConnector derives database/sql connections from a pgxpool.Pool and
// ties the pool's lifetime to the *sql.DB: database/sql calls Close on a
// connector that implements io.Closer when the *sql.DB closes, which is how
// db.Close() on a pool-backed rio.DB releases the pool too.
type poolConnector struct {
	driver.Connector // stdlib's pool connector: Connect acquires, Driver reports
	pool             *pgxpool.Pool
}

// Close closes the pool. pgxpool.Close blocks until every acquired
// connection is returned and is a no-op when repeated, so either closing
// order of view and pool stays safe.
func (c poolConnector) Close() error {
	c.pool.Close()
	return nil
}

// pools associates a pool-built *rio.DB with its pgxpool.Pool for PoolOf.
// The association lives here rather than in the rio core: core stays free of
// driver-native concepts until the native execution channel introduces its
// handle formally. Keys are weak pointers with a GC cleanup, so an abandoned
// DB never pins its map entry (the pool itself stays alive through its own
// background goroutines until closed, as with any un-closed pgxpool).
var pools struct {
	sync.RWMutex
	m map[weak.Pointer[rio.DB]]*pgxpool.Pool
}

func registerPool(db *rio.DB, pool *pgxpool.Pool) {
	key := weak.Make(db)
	pools.Lock()
	if pools.m == nil {
		pools.m = make(map[weak.Pointer[rio.DB]]*pgxpool.Pool)
	}
	pools.m[key] = pool
	pools.Unlock()
	runtime.AddCleanup(db, func(k weak.Pointer[rio.DB]) {
		pools.Lock()
		delete(pools.m, k)
		pools.Unlock()
	}, key)
}
