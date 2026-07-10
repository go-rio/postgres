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
)

// --- Unit tests (no server) --------------------------------------------------

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
		{nil, pgx.TxOptions{}},
		{&sql.TxOptions{}, pgx.TxOptions{}},
		{&sql.TxOptions{Isolation: sql.LevelReadUncommitted}, pgx.TxOptions{IsoLevel: pgx.ReadUncommitted}},
		{&sql.TxOptions{Isolation: sql.LevelReadCommitted}, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}},
		{&sql.TxOptions{Isolation: sql.LevelRepeatableRead}, pgx.TxOptions{IsoLevel: pgx.RepeatableRead}},
		{&sql.TxOptions{Isolation: sql.LevelSnapshot}, pgx.TxOptions{IsoLevel: pgx.RepeatableRead}},
		{&sql.TxOptions{Isolation: sql.LevelSerializable, ReadOnly: true}, pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadOnly}},
	}
	for _, tc := range cases {
		got, err := mapTxOptions(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("mapTxOptions(%+v) = %+v, %v; want %+v", tc.in, got, err, tc.want)
		}
	}
	// The same refusal pgx's database/sql adapter gives.
	if _, err := mapTxOptions(&sql.TxOptions{Isolation: sql.LevelLinearizable}); err == nil || !strings.Contains(err.Error(), "unsupported isolation") {
		t.Errorf("unsupported isolation must be refused, got %v", err)
	}
}

func TestDoneAsTxDone(t *testing.T) {
	err := doneAsTxDone(fmt.Errorf("wrapped: %w", pgx.ErrTxClosed))
	if !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("pgx.ErrTxClosed must translate to sql.ErrTxDone, got %v", err)
	}
	if !errors.Is(err, pgx.ErrTxClosed) {
		t.Fatalf("the pgx sentinel must stay in the chain, got %v", err)
	}
	other := errors.New("network down")
	if got := doneAsTxDone(other); got != other {
		t.Fatalf("unrelated errors must pass through, got %v", got)
	}
	if doneAsTxDone(nil) != nil {
		t.Fatal("nil must stay nil")
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
	// The documented reverse order stays safe: another close is a no-op.
	pool.Close()
	if err := db.Close(); err != nil {
		t.Errorf("repeated Close: %v", err)
	}
}

// --- Integration tests (real PostgreSQL, gated by RIO_POSTGRES_DSN) ----------

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
		if got.Email != "n1@x" || got.Age != 30 || string(got.Blob) != "\x01\x02\x03" || got.Note == nil || *got.Note != "hello" {
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
		// Native queries acquire from the pool without touching the
		// database/sql view: its stats stay at zero open connections after a
		// query storm.
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
			// The pgx.Tx is live and shares the transaction: a native
			// CopyFrom-style call sees rio's uncommitted write.
			u := &nativeProbeUser{Email: "sp@x", Age: 7}
			if err := rio.Insert(ctx, tx, u); err != nil {
				return err
			}
			var n int64
			if err := ptx.QueryRow(ctx, "SELECT count(*) FROM rio_pg_native_probe_users WHERE email = 'sp@x'").Scan(&n); err != nil {
				return err
			}
			if n != 1 {
				t.Errorf("TxOf must share the transaction, count = %d", n)
			}
			// Savepoint choreography with an aborted inner statement: the
			// outer transaction must stay usable (ROLLBACK TO recovery).
			spErr := tx.Tx(ctx, func(sp *rio.Tx) error {
				_, err := rio.Exec(ctx, sp, "INSERT INTO rio_pg_native_probe_users (email, age, created_at, updated_at) VALUES ('sp@x', 1, now(), now())")
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
		// The savepoint's write must have been rolled back, the outer commit kept.
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
