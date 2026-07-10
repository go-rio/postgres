package postgres

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/go-rio/rio"
	"github.com/jackc/pgx/v5/pgxpool"
)

// --- Unit tests (no server) --------------------------------------------------

// lazyPool builds a pool against an address nothing listens on — pgxpool
// never connects eagerly, so construction always succeeds.
func lazyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://user:pass@localhost:1/nowhere?sslmode=disable")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	return pool
}

func TestOpenPoolInvalidDSN(t *testing.T) {
	_, err := OpenPool(context.Background(), "port=not-a-number")
	if err == nil {
		t.Fatal("OpenPool with an invalid DSN should fail")
	}
	if !strings.HasPrefix(err.Error(), "postgres: open pool:") {
		t.Errorf("error %q should carry the package prefix", err)
	}
}

func TestOpenPoolRejectsNonConformingStrings(t *testing.T) {
	// The full spelling matrix is nonConformingStringsSetting's own suite;
	// here each route into the shared guard is proven once for OpenPool.
	tests := []struct {
		name string
		dsn  string
	}{
		{"URL parameter",
			"postgres://user:pass@localhost:5432/app?standard_conforming_strings=off"},
		{"options -c",
			"host=localhost dbname=app options='-c standard_conforming_strings=off'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, err := OpenPool(context.Background(), tt.dsn)
			if err == nil {
				_ = db.Close()
				t.Fatalf("OpenPool(%q) accepted standard_conforming_strings=off", tt.dsn)
			}
			if !strings.Contains(err.Error(), "standard_conforming_strings") {
				t.Errorf("OpenPool(%q) error %q does not name standard_conforming_strings", tt.dsn, err)
			}
			if !strings.HasPrefix(err.Error(), "postgres: open pool:") {
				t.Errorf("error %q should carry the open pool prefix", err)
			}
		})
	}
}

func TestOpenPoolRejectsNonConformingStringsFromPGOPTIONS(t *testing.T) {
	t.Setenv("PGOPTIONS", "-c standard_conforming_strings=off")
	db, err := OpenPool(context.Background(), "postgres://user:pass@localhost:5432/app")
	if err == nil {
		_ = db.Close()
		t.Fatal("OpenPool accepted standard_conforming_strings=off from PGOPTIONS")
	}
	if !strings.Contains(err.Error(), "standard_conforming_strings") {
		t.Errorf("OpenPool error %q does not name standard_conforming_strings", err)
	}
}

func TestOpenPoolDoesNotConnect(t *testing.T) {
	// Like Open, OpenPool only validates: nothing listens on this address and
	// construction must still succeed (pgxpool connects lazily).
	db, err := OpenPool(context.Background(), "postgres://user:pass@localhost:1/nowhere?sslmode=disable")
	if err != nil {
		t.Fatalf("OpenPool: %v", err)
	}
	if PoolOf(db) == nil {
		t.Error("PoolOf should return the pool OpenPool built")
	}
	if err := db.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestPoolOfIdentity(t *testing.T) {
	pool := lazyPool(t)
	db := NewFromPool(pool)
	defer func() { _ = db.Close() }()
	if got := PoolOf(db); got != pool {
		t.Fatalf("PoolOf = %p, want the pool passed to NewFromPool (%p)", got, pool)
	}
}

func TestPoolOfNilOffPoolConstructions(t *testing.T) {
	if got := PoolOf(nil); got != nil {
		t.Errorf("PoolOf(nil) = %p, want nil", got)
	}
	db, err := Open("postgres://user:pass@localhost:1/nowhere?sslmode=disable")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if got := PoolOf(db); got != nil {
		t.Errorf("PoolOf on an Open-built DB = %p, want nil (no pool to manage)", got)
	}
}

func TestNewFromPoolNilPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewFromPool(nil) should panic like rio.New(nil)")
		}
	}()
	_ = NewFromPool(nil)
}

func TestCloseClosesPool(t *testing.T) {
	// The takeover contract without a server: after db.Close() the pool
	// rejects acquisition with pgxpool's closed-pool error rather than a
	// connection failure.
	pool := lazyPool(t)
	db := NewFromPool(pool)
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// pgxpool surfaces puddle's "closed pool" error; the sentinel is not
	// re-exported, so the message is the stable signal.
	err := pool.Ping(context.Background())
	if err == nil || !strings.Contains(err.Error(), "closed pool") {
		t.Fatalf("after db.Close() the pool should be closed, Ping = %v", err)
	}
	// The documented reverse order stays safe: another close is a no-op.
	pool.Close()
	if err := db.Close(); err != nil {
		t.Errorf("repeated Close: %v", err)
	}
}

// --- Integration tests (real PostgreSQL, gated by RIO_POSTGRES_DSN) ----------

func TestPoolIntegration(t *testing.T) {
	dsn := os.Getenv("RIO_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set RIO_POSTGRES_DSN to run against a real PostgreSQL server")
	}
	ctx := context.Background()

	db, err := OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenPool: %v", err)
	}
	pool := PoolOf(db)
	if pool == nil {
		t.Fatal("PoolOf returned nil for an OpenPool-built DB")
	}
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("pool ping %s: %v", dsn, err)
	}

	t.Run("QueriesShareThePool", func(t *testing.T) {
		before := pool.Stat().AcquireCount()
		ns, err := rio.Raw[int64]("SELECT 41 + ?", 1).All(ctx, db)
		if err != nil {
			t.Fatalf("Raw through the view: %v", err)
		}
		if len(ns) != 1 || ns[0] != 42 {
			t.Fatalf("SELECT 41 + 1 = %v, want [42]", ns)
		}
		if after := pool.Stat().AcquireCount(); after <= before {
			t.Errorf("a view query should acquire from the pgx pool: acquires %d -> %d", before, after)
		}
		if err := db.Unwrap().PingContext(ctx); err != nil {
			t.Errorf("Unwrap ping: %v", err)
		}
	})

	t.Run("TranslatorInstalled", func(t *testing.T) {
		// Force a unique violation through the pool-backed handle: the
		// module's translator must map it exactly as with Open.
		for _, stmt := range []string{
			"DROP TABLE IF EXISTS rio_pg_pool_users",
			"CREATE TABLE rio_pg_pool_users (id bigint PRIMARY KEY)",
			"INSERT INTO rio_pg_pool_users (id) VALUES (1)",
		} {
			if _, err := rio.Exec(ctx, db, stmt); err != nil {
				t.Fatalf("setup %q: %v", stmt, err)
			}
		}
		_, err := rio.Exec(ctx, db, "INSERT INTO rio_pg_pool_users (id) VALUES (?)", 1)
		if !errors.Is(err, rio.ErrDuplicateKey) {
			t.Fatalf("err = %v, want rio.ErrDuplicateKey", err)
		}
		if _, err := rio.Exec(ctx, db, "DROP TABLE rio_pg_pool_users"); err != nil {
			t.Fatalf("teardown: %v", err)
		}
	})

	t.Run("CloseClosesPool", func(t *testing.T) {
		if err := db.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if err := pool.Ping(ctx); err == nil || !strings.Contains(err.Error(), "closed pool") {
			t.Fatalf("after db.Close() the pool should be closed, Ping = %v", err)
		}
	})

	t.Run("PoolClosedFirstFailsLoudly", func(t *testing.T) {
		// The documented reverse order: a pool closed out from under the view
		// makes queries fail with the closed-pool error, never silently.
		db2, err := OpenPool(ctx, dsn)
		if err != nil {
			t.Fatalf("OpenPool: %v", err)
		}
		PoolOf(db2).Close()
		if _, err := rio.Raw[int64]("SELECT 1").All(ctx, db2); err == nil || !strings.Contains(err.Error(), "closed pool") {
			t.Errorf("query on a closed pool = %v, want the closed pool error", err)
		}
		if err := db2.Close(); err != nil {
			t.Errorf("Close after the pool is gone: %v", err)
		}
	})
}
