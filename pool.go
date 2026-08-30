package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"

	"github.com/go-rio/rio"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
)

// OpenPool validates a pgxpool DSN and returns a rio DB using pgx's
// database/sql adapter over that pool. It connects lazily; use
// PoolOf(db).Ping(ctx) to verify connectivity. Like Open, it rejects
// standard_conforming_strings=off.
//
// Configure pooling through the DSN, not db.Unwrap(). Closing the rio DB also
// closes the pool and may block until acquired connections are returned.
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

// NewFromPool wraps a caller-built pgxpool.Pool and takes ownership of it.
// The caller must ensure standard_conforming_strings is on. Closing the rio
// DB closes the pool; do not use this function if the pool must outlive it.
func NewFromPool(pool *pgxpool.Pool, opts ...rio.Option) *rio.DB {
	if pool == nil {
		panic("postgres: NewFromPool: pool must not be nil")
	}
	view := sql.OpenDB(poolConnector{Connector: stdlib.GetPoolConnector(pool), pool: pool})
	// An idle database/sql connection would pin a pool connection.
	view.SetMaxIdleConns(0)
	// The handle rides on the DB itself (rio.WithDriverHandle), so PoolOf is
	// a plain assertion and the pool's lifecycle needs no side registry.
	return New(view, append([]rio.Option{rio.WithDriverHandle(pool)}, opts...)...)
}

// PoolOf returns the pool managed by a pool-backed or native rio DB. It
// returns nil for other constructions and for a nil DB.
func PoolOf(db *rio.DB) *pgxpool.Pool {
	if db == nil {
		return nil
	}
	pool, _ := db.DriverHandle().(*pgxpool.Pool)
	return pool
}

// poolConnector makes the database/sql view own the underlying pool.
type poolConnector struct {
	driver.Connector // stdlib's pool connector: Connect acquires, Driver reports
	pool             *pgxpool.Pool
}

func (c poolConnector) Close() error {
	c.pool.Close()
	return nil
}
