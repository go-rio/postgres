package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/go-rio/rio"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
)

// OpenNative builds a pgxpool.Pool from the DSN and wraps it in a *rio.DB
// that executes through pgx natively — no database/sql layer on the query
// path. Every rio semantic is unchanged: same rendered SQL, same scanning
// rules, same errors, same hooks, same savepoints. What changes is the cost:
// the driver.Value boxing tax is gone, so the read path allocates a fraction
// of the stdlib channel's count (see the README's tier table for measured
// numbers).
//
// The DSN accepts everything OpenPool's does, including pgxpool's pool_*
// parameters, and rejects standard_conforming_strings=off exactly as in
// Open. Query execution mode is pgx's own default (QueryExecModeCacheStatement,
// automatic per-connection statement caching); tune it through the DSN
// parameter default_query_exec_mode — behind an old transaction-pooling
// PgBouncer, set default_query_exec_mode=exec (the README has the matrix).
// Like the other constructors, OpenNative validates eagerly but does not
// connect; use PoolOf(db).Ping(ctx) to verify connectivity.
//
// Two public API differences against the stdlib channels, both loud:
// rio.WithStmtCache panics at construction (statement caching belongs to
// pgx's exec mode here), and Tx.Unwrap returns nil inside transactions (no
// *sql.Tx exists) — use TxOf for the pgx.Tx. db.Unwrap() still works: it
// returns a database/sql view over the same pool for pool-agnostic helpers
// (pings, migrations); never tune pooling on the view.
//
// Closing: db.Close() closes the view and then the pool, blocking until
// acquired connections are returned. PoolOf returns the pool for CopyFrom,
// LISTEN/NOTIFY, Stat, and friends.
func OpenNative(ctx context.Context, dsn string, opts ...rio.Option) (*rio.DB, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: open native: %w", err)
	}
	if bad := nonConformingStringsSetting(cfg.ConnConfig.RuntimeParams); bad != "" {
		return nil, errNonConformingStrings("open native", bad)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: open native: %w", err)
	}
	return NewNativeFromPool(pool, opts...), nil
}

// NewNativeFromPool wraps a caller-built pgxpool.Pool in a native-channel
// *rio.DB, for pools that need a custom pgxpool.Config — tracers,
// AfterConnect hooks, MinConns, or a non-default QueryExecMode on the
// ConnConfig. Everything else matches OpenNative, including the closing
// contract: closing the rio.DB closes the pool you passed in. Keep the pool
// out of NewNativeFromPool if it must outlive the rio.DB.
//
// NewNativeFromPool performs no connection hygiene — the pool is the
// caller's; make sure its sessions run with standard_conforming_strings on
// (the server default since PostgreSQL 9.1), or rio's placeholder rewriting
// can disagree with the server's lexing (see Open).
func NewNativeFromPool(pool *pgxpool.Pool, opts ...rio.Option) *rio.DB {
	if pool == nil {
		panic("postgres: NewNativeFromPool: pool must not be nil")
	}
	merged := make([]rio.Option, 0, len(opts)+1)
	merged = append(merged, rio.WithErrorTranslator(translate))
	merged = append(merged, opts...)
	return rio.NewNative(rio.NativeConfig{
		DB:     &nativeDB{pool: pool},
		Handle: pool,
		// The database/sql view keeps Unwrap working over the same pool. It
		// shares connections with native queries and holds none idle
		// (OpenDBFromPool sets zero idle connections, pgx's documented
		// requirement); closing it never touches the pool.
		SQLView: stdlib.OpenDBFromPool(pool),
	}, rio.Postgres, merged...)
}

// TxOf returns the pgx.Tx behind a native-channel *rio.Tx — the door to
// pgx-only abilities inside a transaction (CopyFrom, LISTEN) — and nil for
// every other construction (on the stdlib channels use tx.Unwrap, which
// carries the *sql.Tx). Savepoint-nested Tx values share the root
// transaction's pgx.Tx, exactly as Unwrap shares the *sql.Tx.
func TxOf(tx *rio.Tx) pgx.Tx {
	if tx == nil {
		return nil
	}
	if nt, ok := tx.NativeTx().(*nativeTx); ok {
		return nt.tx
	}
	return nil
}

// nativeDB adapts a pgxpool.Pool to rio's NativeDB SPI.
type nativeDB struct {
	pool *pgxpool.Pool
}

func (d *nativeDB) Query(ctx context.Context, sqlText string, args []any) (rio.NativeRows, error) {
	rows, err := d.pool.Query(ctx, sqlText, args...)
	return wrapRows(rows, err)
}

func (d *nativeDB) Exec(ctx context.Context, sqlText string, args []any) (int64, error) {
	tag, err := d.pool.Exec(ctx, sqlText, args...)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (d *nativeDB) Begin(ctx context.Context, opts *sql.TxOptions) (rio.NativeTx, error) {
	pgxOpts, err := mapTxOptions(opts)
	if err != nil {
		return nil, err
	}
	tx, err := d.pool.BeginTx(ctx, pgxOpts)
	if err != nil {
		return nil, err
	}
	return &nativeTx{tx: tx}, nil
}

func (d *nativeDB) Close() error {
	d.pool.Close()
	return nil
}

// mapTxOptions maps *sql.TxOptions onto pgx.TxOptions — the same mapping
// pgx's own database/sql adapter applies, so the two channels accept and
// refuse identical option sets.
func mapTxOptions(opts *sql.TxOptions) (pgx.TxOptions, error) {
	var pgxOpts pgx.TxOptions
	if opts == nil {
		return pgxOpts, nil
	}
	switch sql.IsolationLevel(opts.Isolation) {
	case sql.LevelDefault:
	case sql.LevelReadUncommitted:
		pgxOpts.IsoLevel = pgx.ReadUncommitted
	case sql.LevelReadCommitted:
		pgxOpts.IsoLevel = pgx.ReadCommitted
	case sql.LevelRepeatableRead, sql.LevelSnapshot:
		pgxOpts.IsoLevel = pgx.RepeatableRead
	case sql.LevelSerializable:
		pgxOpts.IsoLevel = pgx.Serializable
	default:
		return pgxOpts, fmt.Errorf("unsupported isolation: %v", opts.Isolation)
	}
	if opts.ReadOnly {
		pgxOpts.AccessMode = pgx.ReadOnly
	}
	return pgxOpts, nil
}

// nativeTx adapts one pgx.Tx to rio's NativeTx SPI.
type nativeTx struct {
	tx pgx.Tx
}

func (t *nativeTx) Query(ctx context.Context, sqlText string, args []any) (rio.NativeRows, error) {
	rows, err := t.tx.Query(ctx, sqlText, args...)
	return wrapRows(rows, err)
}

func (t *nativeTx) Exec(ctx context.Context, sqlText string, args []any) (int64, error) {
	tag, err := t.tx.Exec(ctx, sqlText, args...)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (t *nativeTx) Commit(ctx context.Context) error {
	return doneAsTxDone(t.tx.Commit(ctx))
}

func (t *nativeTx) Rollback(ctx context.Context) error {
	return doneAsTxDone(t.tx.Rollback(ctx))
}

// doneAsTxDone translates pgx's finished-transaction sentinel into the SPI's
// contract: rio's cleanup paths tolerate exactly sql.ErrTxDone (a begin
// context that died, for instance, makes pgx destroy the transaction on its
// own — semantically identical to database/sql's begin watcher). The pgx
// error stays in the chain for errors.As.
func doneAsTxDone(err error) error {
	if err != nil && errors.Is(err, pgx.ErrTxClosed) {
		return fmt.Errorf("%w (%w)", sql.ErrTxDone, err)
	}
	return err
}

// wrapRows finishes a Query: on error it closes the non-nil Rows pgx returns
// alongside (its contract), and on success it preloads the first row. The
// preload is how pgx's own database/sql adapter behaves, and it is load-
// bearing: under statement caching pgx defers a cached statement's execution
// error to the first Next, but rio's error-translation and hook boundary is
// the Query call — without the preload, a duplicate-key INSERT ... RETURNING
// would surface as a raw *pgconn.PgError instead of rio.ErrDuplicateKey.
func wrapRows(rows pgx.Rows, err error) (rio.NativeRows, error) {
	if err != nil {
		if rows != nil {
			rows.Close()
		}
		return nil, err
	}
	if rows.Next() {
		return &nativeRows{rows: rows, preloaded: true}, nil
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	// A genuinely empty result: pgx already auto-closed at exhaustion, and
	// further Next calls stay false.
	return &nativeRows{rows: rows}, nil
}

// nativeRows adapts pgx.Rows to rio's NativeRows. On the first Scan it
// translates rio's NativeCell dests into per-kind cells implementing exactly
// one pgtype scanner interface each (a cell implementing several would let
// the wrong codec claim it: numeric would feed Float64Scanner on a string
// field, jsonb would feed TextScanner), plus the cell's own sql.Scanner as
// the fallback for column types with no typed route — pgx's fallback decodes
// to the same driver-canonical values database/sql delivers, so degradation
// is slower, never different. rio passes the same dest slots for every row
// of one result (the SPI contract), so the translated slice is built once
// and reused across rows; pgx caches its scan plans per column by dest type
// the same way.
type nativeRows struct {
	rows      pgx.Rows
	cols      []string
	dests     []any
	preloaded bool // first row already fetched by wrapRows, not yet served
}

func (r *nativeRows) Columns() []string {
	if r.cols == nil {
		fds := r.rows.FieldDescriptions()
		cols := make([]string, len(fds))
		for i := range fds {
			cols[i] = fds[i].Name
		}
		r.cols = cols
	}
	return r.cols
}

func (r *nativeRows) Next() bool {
	if r.preloaded {
		r.preloaded = false
		return true
	}
	return r.rows.Next()
}

func (r *nativeRows) Err() error { return r.rows.Err() }
func (r *nativeRows) Close()     { r.rows.Close() }

func (r *nativeRows) Scan(dest ...any) error {
	if r.dests == nil {
		r.translate(dest)
	}
	return r.rows.Scan(r.dests...)
}

func (r *nativeRows) translate(dest []any) {
	fds := r.rows.FieldDescriptions()
	out := make([]any, len(dest))
	for i, d := range dest {
		cell, ok := d.(rio.NativeCell)
		if !ok {
			out[i] = d // a plain pointer: pgx scans it natively
			continue
		}
		kind := cell.ScanKind()
		// numeric into an unsigned field goes through the fallback: the
		// typed route is Int64Scanner, which refuses the upper half of
		// uint64's range, while the stdlib channel receives numeric as a
		// decimal string and accepts it. The fallback delivers that same
		// string; bigint cannot hold those values, so numeric is the only
		// such column type.
		if kind == rio.NativeKindUint && int(fds[i].DataTypeOID) == pgtype.NumericOID {
			kind = rio.NativeKindScanner
		}
		switch kind {
		case rio.NativeKindInt, rio.NativeKindUint:
			out[i] = &intCell{cell}
		case rio.NativeKindFloat:
			out[i] = &floatCell{cell}
		case rio.NativeKindBool:
			out[i] = &boolCell{cell}
		case rio.NativeKindString:
			out[i] = &stringCell{cell}
		case rio.NativeKindBytes, rio.NativeKindJSON:
			// json/jsonb route BytesScanner; rio's SetBytes feeds the JSON
			// decoder directly and copies for byte fields.
			out[i] = &bytesCell{cell}
		case rio.NativeKindTime:
			out[i] = &timeCell{cell}
		default:
			// NativeKindScanner and every kind newer than this adapter:
			// the cell itself is an sql.Scanner and accepts the fallback's
			// driver-canonical values.
			out[i] = cell
		}
	}
	r.dests = out
}

// The per-kind cells forward unboxed values into the rio cell's typed sinks;
// NULL forwards to SetNull, where rio's own NULL rules (pointer nil-out,
// softdelete zero time, loud errors) live. Scan is the shared fallback for
// every column type without a typed route.

type intCell struct{ c rio.NativeCell }

func (c *intCell) ScanInt64(v pgtype.Int8) error {
	if !v.Valid {
		return c.c.SetNull()
	}
	return c.c.SetInt64(v.Int64)
}
func (c *intCell) Scan(src any) error { return c.c.Scan(src) }

type floatCell struct{ c rio.NativeCell }

func (c *floatCell) ScanFloat64(v pgtype.Float8) error {
	if !v.Valid {
		return c.c.SetNull()
	}
	return c.c.SetFloat64(v.Float64)
}
func (c *floatCell) Scan(src any) error { return c.c.Scan(src) }

type boolCell struct{ c rio.NativeCell }

func (c *boolCell) ScanBool(v pgtype.Bool) error {
	if !v.Valid {
		return c.c.SetNull()
	}
	return c.c.SetBool(v.Bool)
}
func (c *boolCell) Scan(src any) error { return c.c.Scan(src) }

type stringCell struct{ c rio.NativeCell }

func (c *stringCell) ScanText(v pgtype.Text) error {
	if !v.Valid {
		return c.c.SetNull()
	}
	return c.c.SetString(v.String)
}
func (c *stringCell) Scan(src any) error { return c.c.Scan(src) }

type bytesCell struct{ c rio.NativeCell }

func (c *bytesCell) ScanBytes(v []byte) error {
	if v == nil {
		return c.c.SetNull()
	}
	return c.c.SetBytes(v) // driver memory; the sink copies where it stores
}
func (c *bytesCell) Scan(src any) error { return c.c.Scan(src) }

type timeCell struct{ c rio.NativeCell }

func (c *timeCell) ScanTimestamptz(v pgtype.Timestamptz) error {
	if !v.Valid {
		return c.c.SetNull()
	}
	if v.InfinityModifier != pgtype.Finite {
		return c.setInfinity(v.InfinityModifier)
	}
	return c.c.SetTime(v.Time)
}

func (c *timeCell) ScanTimestamp(v pgtype.Timestamp) error {
	if !v.Valid {
		return c.c.SetNull()
	}
	if v.InfinityModifier != pgtype.Finite {
		return c.setInfinity(v.InfinityModifier)
	}
	return c.c.SetTime(v.Time)
}

func (c *timeCell) ScanDate(v pgtype.Date) error {
	if !v.Valid {
		return c.c.SetNull()
	}
	if v.InfinityModifier != pgtype.Finite {
		return c.setInfinity(v.InfinityModifier)
	}
	return c.c.SetTime(v.Time)
}

// setInfinity hands the infinity text through the string sink — the exact
// value the stdlib channel's driver.Value carries for infinity timestamps —
// so both channels fail with rio's same cannot-parse error.
func (c *timeCell) setInfinity(m pgtype.InfinityModifier) error {
	return c.c.SetString(m.String())
}

func (c *timeCell) Scan(src any) error { return c.c.Scan(src) }
