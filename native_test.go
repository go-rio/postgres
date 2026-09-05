package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-rio/rio"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestOpenNativeInvalidDSN(t *testing.T) {
	if _, err := OpenNative(context.Background(), "postgres://u@host:not-a-port/db"); err == nil {
		t.Fatal("OpenNative must validate the DSN eagerly")
	}
}

func TestOpenNativeRejectsNonConformingStrings(t *testing.T) {
	_, err := OpenNative(context.Background(), "postgres://u:p@localhost:1/app?standard_conforming_strings=off")
	if err == nil || !strings.Contains(err.Error(), "standard_conforming_strings") {
		t.Fatalf("err = %v, want the conforming-strings refusal", err)
	}
	if !strings.Contains(err.Error(), "open native") {
		t.Fatalf("err = %v, want the open native operation name", err)
	}
}

func TestNewNativeFromPoolNilPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewNativeFromPool(nil) should panic like rio.New(nil)")
		}
	}()
	_ = NewNativeFromPool(nil)
}

func TestNativeWithStmtCachePanics(t *testing.T) {
	pool := lazyPool(t)
	defer pool.Close()
	defer func() {
		p := recover()
		if p == nil {
			t.Fatal("WithStmtCache on the native channel must panic at construction")
		}
		if s, ok := p.(string); !ok || !strings.Contains(s, "default_query_exec_mode") {
			t.Fatalf("panic must point at pgx's exec mode, got %v", p)
		}
	}()
	_ = NewNativeFromPool(pool, rio.WithStmtCache())
}

func TestMapTxOptions(t *testing.T) {
	cases := []struct {
		in   *sql.TxOptions
		want pgx.TxOptions
	}{
		{in: nil, want: pgx.TxOptions{}},
		{in: &sql.TxOptions{}, want: pgx.TxOptions{}},
		{
			in:   &sql.TxOptions{Isolation: sql.LevelReadUncommitted},
			want: pgx.TxOptions{IsoLevel: pgx.ReadUncommitted},
		},
		{
			in:   &sql.TxOptions{Isolation: sql.LevelReadCommitted},
			want: pgx.TxOptions{IsoLevel: pgx.ReadCommitted},
		},
		{
			in:   &sql.TxOptions{Isolation: sql.LevelRepeatableRead},
			want: pgx.TxOptions{IsoLevel: pgx.RepeatableRead},
		},
		{
			in:   &sql.TxOptions{Isolation: sql.LevelSnapshot},
			want: pgx.TxOptions{IsoLevel: pgx.RepeatableRead},
		},
		{
			in: &sql.TxOptions{
				Isolation: sql.LevelSerializable,
				ReadOnly:  true,
			},
			want: pgx.TxOptions{
				IsoLevel:   pgx.Serializable,
				AccessMode: pgx.ReadOnly,
			},
		},
	}
	for _, tc := range cases {
		got, err := mapTxOptions(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("mapTxOptions(%+v) = %+v, %v; want %+v", tc.in, got, err, tc.want)
		}
	}
	_, err := mapTxOptions(&sql.TxOptions{Isolation: sql.LevelLinearizable})
	if err == nil || !strings.Contains(err.Error(), "unsupported isolation") {
		t.Errorf("unsupported isolation must be refused, got %v", err)
	}
}

func TestDoneAsTxDone(t *testing.T) {
	err := doneAsTxDone(fmt.Errorf("wrapped: %w", pgx.ErrTxClosed), false)
	if !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("pgx.ErrTxClosed must translate to sql.ErrTxDone, got %v", err)
	}
	if !errors.Is(err, pgx.ErrTxClosed) {
		t.Fatalf("the pgx sentinel must stay in the chain, got %v", err)
	}
	other := errors.New("network down")
	if got := doneAsTxDone(other, false); got != other {
		t.Fatalf("unrelated errors must pass through, got %v", got)
	}
	closed := errors.New("connection closed")
	got := doneAsTxDone(closed, true)
	if !errors.Is(got, sql.ErrTxDone) || !errors.Is(got, closed) {
		t.Fatalf("an error from an already closed connection must preserve both errors, got %v", got)
	}
	if doneAsTxDone(nil, true) != nil {
		t.Fatal("nil must stay nil")
	}
}

type adapterModel struct {
	ID     int64
	Rate   float64
	Active bool
	Name   string
	Blob   []byte
	At     time.Time
	Big    uint64
}

type adapterNativeDB struct{ checked *bool }

func (d adapterNativeDB) Query(context.Context, string, []any) (rio.NativeRows, error) {
	return &adapterRows{checked: d.checked}, nil
}
func (adapterNativeDB) Exec(context.Context, string, []any) (int64, error) { return 0, nil }
func (adapterNativeDB) Begin(context.Context, *sql.TxOptions) (rio.NativeTx, error) {
	return nil, errors.New("not used")
}
func (adapterNativeDB) Close() error { return nil }

type adapterRows struct {
	checked *bool
	pos     int
}

func (r *adapterRows) Columns() []string {
	return []string{"id", "rate", "active", "name", "blob", "at", "big"}
}
func (r *adapterRows) Next() bool { r.pos++; return r.pos == 1 }
func (r *adapterRows) Err() error { return nil }
func (r *adapterRows) Close()     {}
func (r *adapterRows) Scan(dest ...any) error {
	fields := []pgconn.FieldDescription{
		{DataTypeOID: pgtype.Int8OID},
		{DataTypeOID: pgtype.Float8OID},
		{DataTypeOID: pgtype.BoolOID},
		{DataTypeOID: pgtype.TextOID},
		{DataTypeOID: pgtype.ByteaOID},
		{DataTypeOID: pgtype.TimestamptzOID},
		{DataTypeOID: pgtype.NumericOID},
	}
	translated := &nativeRows{rows: fieldRows{fields: fields}}
	translated.translate(dest)
	if len(translated.cells) != len(dest) || len(translated.dests) != len(dest) {
		return fmt.Errorf("adapter backing: %d cells, %d dests", len(translated.cells), len(translated.dests))
	}
	if translated.dests[0] != (*intCell)(&translated.cells[0]) ||
		translated.dests[1] != (*floatCell)(&translated.cells[1]) ||
		translated.dests[2] != (*boolCell)(&translated.cells[2]) ||
		translated.dests[3] != (*stringCell)(&translated.cells[3]) ||
		translated.dests[4] != (*bytesCell)(&translated.cells[4]) ||
		translated.dests[5] != (*timeCell)(&translated.cells[5]) {
		return errors.New("typed destinations are not views into the shared backing array")
	}
	if translated.dests[6] != dest[6] {
		return errors.New("numeric uint must keep the scanner fallback")
	}
	if err := translated.dests[0].(*intCell).ScanInt64(pgtype.Int8{Int64: 7, Valid: true}); err != nil {
		return err
	}
	if err := translated.dests[1].(*floatCell).ScanFloat64(pgtype.Float8{Float64: 1.5, Valid: true}); err != nil {
		return err
	}
	if err := translated.dests[2].(*boolCell).ScanBool(pgtype.Bool{Bool: true, Valid: true}); err != nil {
		return err
	}
	if err := translated.dests[3].(*stringCell).ScanText(pgtype.Text{String: "rio", Valid: true}); err != nil {
		return err
	}
	if err := translated.dests[4].(*bytesCell).ScanBytes([]byte{1, 2, 3}); err != nil {
		return err
	}
	now := time.Date(2026, 8, 2, 1, 2, 3, 0, time.UTC)
	if err := translated.dests[5].(*timeCell).ScanTimestamptz(pgtype.Timestamptz{Time: now, Valid: true}); err != nil {
		return err
	}
	if err := dest[6].(rio.NativeCell).Scan("18446744073709551615"); err != nil {
		return err
	}
	*r.checked = true
	return nil
}

type fieldRows struct{ fields []pgconn.FieldDescription }

func (fieldRows) Close()                                         {}
func (fieldRows) Err() error                                     { return nil }
func (fieldRows) CommandTag() pgconn.CommandTag                  { return pgconn.CommandTag{} }
func (r fieldRows) FieldDescriptions() []pgconn.FieldDescription { return r.fields }
func (fieldRows) Next() bool                                     { return false }
func (fieldRows) Scan(...any) error                              { return errors.New("not used") }
func (fieldRows) Values() ([]any, error)                         { return nil, errors.New("not used") }
func (fieldRows) RawValues() [][]byte                            { return nil }
func (fieldRows) Conn() *pgx.Conn                                { return nil }

func TestNativeAdaptersShareBackingArray(t *testing.T) {
	checked := false
	db := rio.NewNative(rio.NativeConfig{DB: adapterNativeDB{checked: &checked}}, rio.Postgres)
	rows, err := rio.From[adapterModel]().All(context.Background(), db)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if !checked || len(rows) != 1 {
		t.Fatalf("adapter was not exercised: checked=%v rows=%d", checked, len(rows))
	}
	got := rows[0]
	if got.ID != 7 ||
		got.Rate != 1.5 ||
		!got.Active ||
		got.Name != "rio" ||
		string(got.Blob) != "\x01\x02\x03" ||
		got.Big != ^uint64(0) {
		t.Fatalf("typed adapter values: %+v", got)
	}
}

func TestPoolOfNativeIdentity(t *testing.T) {
	pool := lazyPool(t)
	db := NewNativeFromPool(pool)
	defer func() { _ = db.Close() }()
	if got := PoolOf(db); got != pool {
		t.Fatalf("PoolOf = %p, want the pool passed to NewNativeFromPool (%p)", got, pool)
	}
	if db.Native() != any(pool) {
		t.Fatal("rio's Native() must carry the pool handle verbatim")
	}
	if db.Unwrap() == nil {
		t.Fatal("Unwrap must return the database/sql view on the native channel")
	}
	if TxOf(nil) != nil {
		t.Fatal("TxOf(nil) must be nil")
	}
}

func TestNativeCloseClosesPoolAndView(t *testing.T) {
	pool := lazyPool(t)
	db := NewNativeFromPool(pool)
	view := db.Unwrap()
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := pool.Ping(context.Background()); err == nil || !strings.Contains(err.Error(), "closed pool") {
		t.Fatalf("after db.Close() the pool should be closed, Ping = %v", err)
	}
	if err := view.PingContext(context.Background()); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("after db.Close() the view should be closed, Ping = %v", err)
	}
	pool.Close()
	if err := db.Close(); err != nil {
		t.Errorf("repeated Close: %v", err)
	}
}

type nativeProbeUser struct {
	ID        int64
	Email     string
	Age       int64
	Blob      []byte
	Note      *string
	DeletedAt *time.Time `rio:",softdelete"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (nativeProbeUser) TableName() string { return "rio_pg_native_probe_users" }

func TestNativeIntegration(t *testing.T) {
	dsn := os.Getenv("RIO_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set RIO_POSTGRES_DSN to run against a real PostgreSQL server")
	}
	ctx := context.Background()

	db, err := OpenNative(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenNative: %v", err)
	}
	defer func() { _ = db.Close() }()
	pool := PoolOf(db)
	if pool == nil {
		t.Fatal("PoolOf returned nil for an OpenNative-built DB")
	}
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("pool ping %s: %v", dsn, err)
	}

	for _, stmt := range []string{
		"DROP TABLE IF EXISTS rio_pg_native_probe_users",
		`CREATE TABLE rio_pg_native_probe_users (
			id bigserial PRIMARY KEY,
			email text NOT NULL UNIQUE,
			age bigint NOT NULL,
			blob bytea,
			note text,
			deleted_at timestamptz,
			created_at timestamptz NOT NULL,
			updated_at timestamptz NOT NULL
		)`,
	} {
		if _, err := rio.Exec(ctx, db, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}
	defer func() { _, _ = rio.Exec(ctx, db, "DROP TABLE IF EXISTS rio_pg_native_probe_users") }()

	t.Run("CRUDRoundTrip", func(t *testing.T) {
		note := "hello"
		u := &nativeProbeUser{Email: "n1@x", Age: 30, Blob: []byte{1, 2, 3}, Note: &note}
		if err := rio.Insert(ctx, db, u); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		if u.ID == 0 {
			t.Fatal("RETURNING must backfill the generated ID")
		}
		got, err := rio.Find[nativeProbeUser](ctx, db, u.ID)
		if err != nil {
			t.Fatalf("Find: %v", err)
		}
		if got.Email != "n1@x" ||
			got.Age != 30 ||
			string(got.Blob) != "\x01\x02\x03" ||
			got.Note == nil ||
			*got.Note != "hello" {
			t.Fatalf("round trip lost data: %+v", got)
		}
		if !got.CreatedAt.Equal(u.CreatedAt) {
			t.Fatalf("timestamps must round-trip Equal: %v vs %v", got.CreatedAt, u.CreatedAt)
		}
		got.Age = 31
		if err := rio.Update(ctx, db, got); err != nil {
			t.Fatalf("Update: %v", err)
		}
		if err := rio.Delete(ctx, db, got); err != nil {
			t.Fatalf("Delete (soft): %v", err)
		}
		if _, err := rio.Find[nativeProbeUser](ctx, db, u.ID); !errors.Is(err, rio.ErrNotFound) {
			t.Fatalf("soft-deleted row must be invisible: %v", err)
		}
		if err := rio.Restore(ctx, db, got); err != nil {
			t.Fatalf("Restore: %v", err)
		}
		if err := rio.ForceDelete(ctx, db, got); err != nil {
			t.Fatalf("ForceDelete: %v", err)
		}
	})

	t.Run("TranslatorInstalled", func(t *testing.T) {
		u1 := &nativeProbeUser{Email: "dup@x", Age: 1}
		if err := rio.Insert(ctx, db, u1); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		err := rio.Insert(ctx, db, &nativeProbeUser{Email: "dup@x", Age: 2})
		if !errors.Is(err, rio.ErrDuplicateKey) {
			t.Fatalf("err = %v, want rio.ErrDuplicateKey", err)
		}
		if err := rio.ForceDelete(ctx, db, u1); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
	})

	t.Run("QueriesBypassTheView", func(t *testing.T) {
		for range 5 {
			if _, err := rio.Raw[int64]("SELECT 1").All(ctx, db); err != nil {
				t.Fatalf("Raw: %v", err)
			}
		}
		if inUse := db.Unwrap().Stats().OpenConnections; inUse != 0 {
			t.Errorf("native queries must not run through the view, it has %d open connections", inUse)
		}
		if err := db.Unwrap().PingContext(ctx); err != nil {
			t.Errorf("the view must still work for pool-agnostic helpers: %v", err)
		}
	})

	t.Run("TxOfAndSavepoints", func(t *testing.T) {
		err := db.Tx(ctx, func(tx *rio.Tx) error {
			if tx.Unwrap() != nil {
				t.Error("Tx.Unwrap must be nil on the native channel")
			}
			ptx := TxOf(tx)
			if ptx == nil {
				t.Fatal("TxOf must return the pgx.Tx")
			}
			u := &nativeProbeUser{Email: "sp@x", Age: 7}
			if err := rio.Insert(ctx, tx, u); err != nil {
				return err
			}
			var n int64
			row := ptx.QueryRow(
				ctx,
				"SELECT count(*) FROM rio_pg_native_probe_users WHERE email = 'sp@x'",
			)
			if err := row.Scan(&n); err != nil {
				return err
			}
			if n != 1 {
				t.Errorf("TxOf must share the transaction, count = %d", n)
			}
			spErr := tx.Tx(ctx, func(sp *rio.Tx) error {
				_, err := rio.Exec(
					ctx,
					sp,
					"INSERT INTO rio_pg_native_probe_users "+
						"(email, age, created_at, updated_at) "+
						"VALUES ('sp@x', 1, now(), now())",
				)
				if err == nil {
					t.Error("duplicate insert inside the savepoint must fail")
				}
				return err
			})
			if !errors.Is(spErr, rio.ErrDuplicateKey) {
				t.Errorf("savepoint must surface the translated error: %v", spErr)
			}
			return rio.ForceDelete(ctx, tx, u)
		})
		if err != nil {
			t.Fatalf("Tx: %v", err)
		}
	})

	t.Run("SavepointCleanupSurvivesCanceledContext", func(t *testing.T) {
		boom := errors.New("inner failed after its ctx died")
		err := db.Tx(ctx, func(tx *rio.Tx) error {
			u := &nativeProbeUser{Email: "cc@x", Age: 1}
			if err := rio.Insert(ctx, tx, u); err != nil {
				return err
			}
			inner, cancel := context.WithCancel(ctx)
			spErr := tx.Tx(inner, func(sp *rio.Tx) error {
				if err := rio.Insert(inner, sp, &nativeProbeUser{Email: "leak@x", Age: 2}); err != nil {
					return err
				}
				cancel()
				return boom
			})
			if !errors.Is(spErr, boom) || errors.Is(spErr, context.Canceled) {
				t.Fatalf("savepoint cleanup must survive the dead context: %v", spErr)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("outer Tx: %v", err)
		}
		leaked, err := rio.From[nativeProbeUser]().Where("email = ?", "leak@x").Exists(ctx, db)
		if err != nil {
			t.Fatalf("Exists: %v", err)
		}
		if leaked {
			t.Fatal("the savepoint's write leaked into the outer commit")
		}
		kept, err := rio.From[nativeProbeUser]().Where("email = ?", "cc@x").Exists(ctx, db)
		if err != nil || !kept {
			t.Fatalf("the outer transaction's write must have committed: %v %v", kept, err)
		}
		if _, err := rio.Exec(ctx, db, "DELETE FROM rio_pg_native_probe_users"); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
	})

	t.Run("WholeTxRollbackOnDeadContext", func(t *testing.T) {
		boom := errors.New("fn failed after killing its ctx")
		inner, cancel := context.WithCancel(ctx)
		err := db.Tx(inner, func(tx *rio.Tx) error {
			if err := rio.Insert(inner, tx, &nativeProbeUser{Email: "dead@x", Age: 1}); err != nil {
				return err
			}
			cancel()
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("want the fn error, got %v", err)
		}
		if strings.Contains(err.Error(), "rollback") {
			t.Fatalf("rollback must have succeeded or been tolerated, got %v", err)
		}
		gone, err2 := rio.From[nativeProbeUser]().Where("email = ?", "dead@x").Exists(ctx, db)
		if err2 != nil || gone {
			t.Fatalf("the transaction's write must be rolled back: exists=%v err=%v", gone, err2)
		}
	})
}

func TestNativeStringScansMatchDatabaseSQL(t *testing.T) {
	dsn := os.Getenv("RIO_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("RIO_POSTGRES_DSN not set")
	}
	ctx := context.Background()
	std, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer std.Close()
	native, err := OpenNative(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()

	for _, expr := range []string{
		"'08:00:00'::time", "'17:30:00.5'::time", "'08:00:00+08'::timetz", "'1 day 02:03:04'::interval",
		"'192.168.0.1'::inet", "'10.0.0.0/8'::inet", "'10.0.0.0/8'::cidr", "'::1'::inet",
		"'12.3400'::numeric", "'{\"a\":1}'::jsonb", "'a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11'::uuid",
	} {
		want, err := rio.Raw[string]("SELECT "+expr).Sole(ctx, std)
		if err != nil {
			t.Fatalf("%s via database/sql: %v", expr, err)
		}
		got, err := rio.Raw[string]("SELECT "+expr).Sole(ctx, native)
		if err != nil {
			t.Fatalf("%s via native: %v", expr, err)
		}
		if *got != *want {
			t.Errorf("%s: native %q, database/sql %q", expr, *got, *want)
		}
	}
}

func TestNativeArrayIntoStringFails(t *testing.T) {
	dsn := os.Getenv("RIO_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("RIO_POSTGRES_DSN not set")
	}
	ctx := context.Background()
	native, err := OpenNative(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()

	_, err = rio.Raw[string]("SELECT ARRAY[1,2]::int[]").Sole(ctx, native)
	if err == nil || !strings.Contains(err.Error(), "slice destination") {
		t.Fatalf("expected a slice-destination error, got %v", err)
	}
}

func TestNewNativeFromPoolWithAfterConnect(t *testing.T) {
	dsn := os.Getenv("RIO_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("RIO_POSTGRES_DSN not set")
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AfterConnect = AfterConnect
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	db := NewNativeFromPool(pool)
	defer db.Close()

	got, err := rio.Raw[string]("SELECT '08:00:00'::time").Sole(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if *got != "08:00:00" {
		t.Fatalf("got %q", *got)
	}
}
