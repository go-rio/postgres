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

// OpenNative validates a pgxpool DSN and returns a rio DB that executes
// queries directly through pgx. It connects lazily; use PoolOf(db).Ping(ctx)
// to verify connectivity. Like Open, it rejects
// standard_conforming_strings=off.
//
// Statement caching is controlled by pgx's default_query_exec_mode;
// rio.WithStmtCache is unsupported. Tx.Unwrap returns nil, so use TxOf for
// the pgx transaction. db.Unwrap returns a database/sql view for compatible
// helpers, but pooling must be configured through pgxpool.
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

// NewNativeFromPool wraps a caller-built pgxpool.Pool for native execution
// and takes ownership of it. The caller must ensure
// standard_conforming_strings is on. Closing the rio DB closes the pool.
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
		// This non-owning view keeps Unwrap available to database/sql helpers.
		SQLView: stdlib.OpenDBFromPool(pool),
	}, rio.Postgres, merged...)
}

// TxOf returns the pgx transaction behind a native rio transaction. It
// returns nil for other constructions and for a nil transaction. Nested
// savepoints share the root pgx transaction.
func TxOf(tx *rio.Tx) pgx.Tx {
	if tx == nil {
		return nil
	}
	if nt, ok := tx.NativeTx().(*nativeTx); ok {
		return nt.tx
	}
	return nil
}

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
	wasClosed := t.tx.Conn().IsClosed()
	return doneAsTxDone(t.tx.Commit(ctx), wasClosed)
}

func (t *nativeTx) Rollback(ctx context.Context) error {
	wasClosed := t.tx.Conn().IsClosed()
	return doneAsTxDone(t.tx.Rollback(ctx), wasClosed)
}

// nativeRows assigns one pgtype scanner interface per rio cell so pgx cannot
// select an incompatible codec. Unsupported kinds fall back to sql.Scanner.
// The translated destinations are reused for every row.
type nativeRows struct {
	rows            pgx.Rows
	cols            []string
	cells           []pgCell
	dests           []any
	hasPreloadedRow bool // wrapRows already fetched the first row
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
	if r.hasPreloadedRow {
		r.hasPreloadedRow = false
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

// pgCell is the shared backing layout for the per-kind adapters. Converting a
// slot pointer to a named view preserves each view's narrow pgx method set.
type pgCell struct{ c rio.NativeCell }

type intCell pgCell

func (c *intCell) ScanInt64(v pgtype.Int8) error {
	if !v.Valid {
		return c.c.SetNull()
	}
	return c.c.SetInt64(v.Int64)
}

func (c *intCell) Scan(src any) error { return c.c.Scan(src) }

type floatCell pgCell

func (c *floatCell) ScanFloat64(v pgtype.Float8) error {
	if !v.Valid {
		return c.c.SetNull()
	}
	return c.c.SetFloat64(v.Float64)
}

func (c *floatCell) Scan(src any) error { return c.c.Scan(src) }

type boolCell pgCell

func (c *boolCell) ScanBool(v pgtype.Bool) error {
	if !v.Valid {
		return c.c.SetNull()
	}
	return c.c.SetBool(v.Bool)
}

func (c *boolCell) Scan(src any) error { return c.c.Scan(src) }

type stringCell pgCell

func (c *stringCell) ScanText(v pgtype.Text) error {
	if !v.Valid {
		return c.c.SetNull()
	}
	return c.c.SetString(v.String)
}

func (c *stringCell) Scan(src any) error { return c.c.Scan(src) }

type bytesCell pgCell

func (c *bytesCell) ScanBytes(v []byte) error {
	if v == nil {
		return c.c.SetNull()
	}
	return c.c.SetBytes(v) // driver memory; the sink copies where it stores
}

func (c *bytesCell) Scan(src any) error { return c.c.Scan(src) }

type timeCell pgCell

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

func (c *timeCell) Scan(src any) error { return c.c.Scan(src) }

// setInfinity follows the database/sql path by passing infinity as text.
func (c *timeCell) setInfinity(m pgtype.InfinityModifier) error {
	return c.c.SetString(m.String())
}

func (r *nativeRows) translate(dest []any) {
	fds := r.rows.FieldDescriptions()
	r.cells = make([]pgCell, len(dest))
	out := make([]any, len(dest))
	for i, d := range dest {
		cell, ok := d.(rio.NativeCell)
		if !ok {
			out[i] = d // a plain pointer: pgx scans it natively
			continue
		}
		kind := cell.ScanKind()
		// Numeric uses its decimal-string fallback to preserve full uint64 range.
		if kind == rio.NativeKindUint && int(fds[i].DataTypeOID) == pgtype.NumericOID {
			kind = rio.NativeKindScanner
		}
		switch kind {
		case rio.NativeKindInt, rio.NativeKindUint:
			r.cells[i].c = cell
			out[i] = (*intCell)(&r.cells[i])
		case rio.NativeKindFloat:
			r.cells[i].c = cell
			out[i] = (*floatCell)(&r.cells[i])
		case rio.NativeKindBool:
			r.cells[i].c = cell
			out[i] = (*boolCell)(&r.cells[i])
		case rio.NativeKindString:
			r.cells[i].c = cell
			out[i] = (*stringCell)(&r.cells[i])
		case rio.NativeKindBytes, rio.NativeKindJSON:
			r.cells[i].c = cell
			out[i] = (*bytesCell)(&r.cells[i])
		case rio.NativeKindTime:
			r.cells[i].c = cell
			out[i] = (*timeCell)(&r.cells[i])
		default:
			// New kinds retain the driver-canonical sql.Scanner fallback.
			out[i] = cell
		}
	}
	r.dests = out
}

// mapTxOptions matches pgx's database/sql isolation mapping.
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

// doneAsTxDone exposes a pgx-closed transaction or connection as sql.ErrTxDone
// while preserving the original error in the chain.
func doneAsTxDone(err error, wasClosed bool) error {
	if err != nil && (wasClosed || errors.Is(err, pgx.ErrTxClosed)) {
		return fmt.Errorf("%w (%w)", sql.ErrTxDone, err)
	}
	return err
}

// wrapRows preloads the first row so errors deferred by pgx statement caching
// remain inside rio's query translation and hook boundary.
func wrapRows(rows pgx.Rows, err error) (rio.NativeRows, error) {
	if err != nil {
		if rows != nil {
			rows.Close()
		}
		return nil, err
	}
	if rows.Next() {
		return &nativeRows{rows: rows, hasPreloadedRow: true}, nil
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	return &nativeRows{rows: rows}, nil
}
