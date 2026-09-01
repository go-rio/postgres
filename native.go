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
// queries directly through pgx. It connects lazily (ping via PoolOf) and,
// like Open, rejects standard_conforming_strings=off.
//
// rio.WithStmtCache is unsupported; use pgx's default_query_exec_mode.
// Tx.Unwrap returns nil — use TxOf. db.Unwrap returns a database/sql view;
// configure pooling through pgxpool, never the view.
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
	// A construction panic (e.g. rio.WithStmtCache) must not leak the pool.
	defer func() {
		if p := recover(); p != nil {
			pool.Close()
			panic(p)
		}
	}()
	return NewNativeFromPool(pool, opts...), nil
}

// NewNativeFromPool wraps a caller-built pgxpool.Pool for native execution
// and takes ownership: closing the rio DB closes the pool. The caller must
// ensure standard_conforming_strings is on.
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
		// Non-owning view backing db.Unwrap.
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

// nativeDB is the pool-backed rio.NativeDB; it also batches preload layers
// and streams InsertAll over COPY.
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

// QueryBatch runs all statements in one round trip; results come back in
// order, each through wrapRows.
func (d *nativeDB) QueryBatch(ctx context.Context, stmts []rio.BatchStatement) (rio.NativeBatchResults, error) {
	return sendBatch(ctx, d.pool, stmts)
}

// CopyIn streams rows over COPY; the table's name segments map onto pgx.Identifier.
func (d *nativeDB) CopyIn(ctx context.Context, table []string, columns []string, next func() ([]any, error)) (int64, error) {
	return d.pool.CopyFrom(ctx, pgx.Identifier(table), columns, pgx.CopyFromFunc(next))
}

// nativeTx is the rio.NativeTx over one pgx transaction.
type nativeTx struct {
	tx pgx.Tx
	// done latches after Commit/Rollback: the connection is released either
	// way and must not be touched again. rio never uses a Tx concurrently.
	done bool
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

func (t *nativeTx) Commit(ctx context.Context) error   { return t.finish(ctx, t.tx.Commit) }
func (t *nativeTx) Rollback(ctx context.Context) error { return t.finish(ctx, t.tx.Rollback) }

// finish runs the terminal operation once; IsClosed is read before op so
// doneAsTxDone can map an already-dead connection.
func (t *nativeTx) finish(ctx context.Context, op func(context.Context) error) error {
	if t.done {
		return fmt.Errorf("%w (%w)", sql.ErrTxDone, pgx.ErrTxClosed)
	}
	wasClosed := t.tx.Conn().IsClosed()
	err := op(ctx)
	t.done = true
	return doneAsTxDone(err, wasClosed)
}

func (t *nativeTx) QueryBatch(ctx context.Context, stmts []rio.BatchStatement) (rio.NativeBatchResults, error) {
	return sendBatch(ctx, t.tx, stmts)
}

func (t *nativeTx) CopyIn(ctx context.Context, table []string, columns []string, next func() ([]any, error)) (int64, error) {
	return t.tx.CopyFrom(ctx, pgx.Identifier(table), columns, pgx.CopyFromFunc(next))
}

// nativeRows assigns one pgtype scanner interface per rio cell so pgx cannot
// select an incompatible codec; unsupported kinds fall back to sql.Scanner.
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

// translate builds the pgx destinations once: one typed cell view per rio
// cell, plain pointers passed through.
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
		// Numeric falls back to decimal strings to keep the full uint64 range.
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
			out[i] = cell
		}
	}
	r.dests = out
}

// pgCell backs the per-kind adapter views; each named view keeps its narrow
// pgx method set.
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

// batchSender is the SendBatch surface shared by pools and transactions.
type batchSender interface {
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}

func sendBatch(ctx context.Context, s batchSender, stmts []rio.BatchStatement) (rio.NativeBatchResults, error) {
	b := &pgx.Batch{}
	for _, st := range stmts {
		b.Queue(st.SQL, st.Args...)
	}
	return &nativeBatchResults{res: s.SendBatch(ctx, b), remaining: len(stmts)}, nil
}

// nativeBatchResults hands out one wrapped result per queued statement.
type nativeBatchResults struct {
	res       pgx.BatchResults
	remaining int
}

func (r *nativeBatchResults) Rows() (rio.NativeRows, bool, error) {
	if r.remaining == 0 {
		return nil, true, nil
	}
	r.remaining--
	nr, err := wrapRows(r.res.Query())
	if err != nil {
		return nil, false, err
	}
	return nr, false, nil
}

func (r *nativeBatchResults) Close() error { return r.res.Close() }

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

// doneAsTxDone exposes a pgx-closed transaction or connection as sql.ErrTxDone,
// keeping the original error in the chain.
func doneAsTxDone(err error, wasClosed bool) error {
	if err != nil && (wasClosed || errors.Is(err, pgx.ErrTxClosed)) {
		return fmt.Errorf("%w (%w)", sql.ErrTxDone, err)
	}
	return err
}

// wrapRows prefetches the first row so pgx-deferred errors surface at the query.
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
