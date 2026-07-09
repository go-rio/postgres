package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-rio/rio"
	"github.com/jackc/pgx/v5/pgconn"
)

// --- Unit tests -------------------------------------------------------------

func TestTranslate(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want error
	}{
		{"unique violation", &pgconn.PgError{Code: "23505"}, rio.ErrDuplicateKey},
		{"foreign key violation", &pgconn.PgError{Code: "23503"}, rio.ErrForeignKeyViolated},
		{"wrapped unique violation", fmt.Errorf("insert users: %w", &pgconn.PgError{Code: "23505"}), rio.ErrDuplicateKey},
		{"deeply wrapped fk violation", fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", &pgconn.PgError{Code: "23503"})), rio.ErrForeignKeyViolated},
		{"unrelated pg error", &pgconn.PgError{Code: "42P01"}, nil},
		{"not null violation is not ours", &pgconn.PgError{Code: "23502"}, nil},
		{"non-pg error", errors.New("connection refused"), nil},
		{"nil error", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// translate returns the sentinel itself, so identity comparison
			// is exact.
			if got := translate(tt.err); got != tt.want {
				t.Errorf("translate(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestOpenInvalidDSN(t *testing.T) {
	_, err := Open("port=not-a-number")
	if err == nil {
		t.Fatal("Open with an invalid DSN should fail")
	}
	if !strings.HasPrefix(err.Error(), "postgres: open:") {
		t.Errorf("error %q should carry the package prefix", err)
	}
}

func TestOpenDoesNotConnect(t *testing.T) {
	// Like sql.Open, Open only validates the DSN; nothing listens on this
	// address and Open must still succeed.
	db, err := Open("postgres://user:pass@localhost:1/nowhere?sslmode=disable")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// stubDriver is a database/sql driver whose every ExecContext fails with a
// *pgconn.PgError carrying the SQLSTATE given as the DSN. It lets the unit
// suite prove that New wires the translator into rio without a server.
type stubDriver struct{}

func (stubDriver) Open(code string) (driver.Conn, error) { return stubConn{code: code}, nil }

type stubConn struct{ code string }

func (stubConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("stub: no prepare") }
func (stubConn) Close() error                        { return nil }
func (stubConn) Begin() (driver.Tx, error)           { return nil, errors.New("stub: no begin") }

func (c stubConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return nil, &pgconn.PgError{Severity: "ERROR", Code: c.code, Message: "stub failure"}
}

func init() { sql.Register("rio-postgres-stub", stubDriver{}) }

func stubDB(t *testing.T, code string, opts ...rio.Option) *rio.DB {
	t.Helper()
	sqlDB, err := sql.Open("rio-postgres-stub", code)
	if err != nil {
		t.Fatalf("open stub: %v", err)
	}
	db := New(sqlDB, opts...)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestNewInstallsTranslator(t *testing.T) {
	tests := []struct {
		code string
		want error
	}{
		{"23505", rio.ErrDuplicateKey},
		{"23503", rio.ErrForeignKeyViolated},
	}
	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			db := stubDB(t, tt.code)
			_, err := rio.Exec(context.Background(), db, "DELETE FROM widgets WHERE id = 1")
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want errors.Is(err, %v)", err, tt.want)
			}
			// The driver error must stay in the chain for errors.As.
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != tt.code {
				t.Fatalf("errors.As should reach the *pgconn.PgError with code %s, got %v", tt.code, err)
			}
		})
	}
}

func TestNewUserTranslatorWins(t *testing.T) {
	custom := errors.New("custom sentinel")
	db := stubDB(t, "42P01", rio.WithErrorTranslator(func(err error) error {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
			return custom
		}
		return nil
	}))
	_, err := rio.Exec(context.Background(), db, "DELETE FROM widgets WHERE id = 1")
	if !errors.Is(err, custom) {
		t.Fatalf("a user-supplied translator should replace the package one; err = %v", err)
	}
}

// --- Integration tests (real PostgreSQL, gated by RIO_POSTGRES_DSN) ---------

type pgUser struct {
	ID        int64
	Email     string
	Nickname  string `rio:",omitzero"` // omitted when zero so the DB default applies
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (pgUser) TableName() string { return "rio_pg_users" }

type pgPost struct {
	ID     int64
	UserID int64
	Title  string
}

func (pgPost) TableName() string { return "rio_pg_posts" }

func TestIntegration(t *testing.T) {
	dsn := os.Getenv("RIO_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set RIO_POSTGRES_DSN to run against a real PostgreSQL server")
	}
	ctx := context.Background()

	db, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Unwrap().PingContext(ctx); err != nil {
		t.Fatalf("ping %s: %v", dsn, err)
	}

	// A fresh schema per run; drop first so a failed run stays inspectable.
	for _, stmt := range []string{
		"DROP TABLE IF EXISTS rio_pg_posts",
		"DROP TABLE IF EXISTS rio_pg_users",
		`CREATE TABLE rio_pg_users (
			id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			email      text NOT NULL UNIQUE,
			nickname   text NOT NULL DEFAULT 'anonymous',
			created_at timestamptz NOT NULL,
			updated_at timestamptz NOT NULL
		)`,
		`CREATE TABLE rio_pg_posts (
			id      bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			user_id bigint NOT NULL REFERENCES rio_pg_users (id),
			title   text NOT NULL
		)`,
	} {
		if _, err := rio.Exec(ctx, db, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}

	var ada pgUser

	t.Run("InsertReturningBackfill", func(t *testing.T) {
		ada = pgUser{Email: "ada@example.com"}
		if err := rio.Insert(ctx, db, &ada); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		if ada.ID == 0 {
			t.Error("RETURNING should backfill the generated primary key")
		}
		if ada.Nickname != "anonymous" {
			t.Errorf("Nickname = %q, want the DB default %q backfilled via RETURNING", ada.Nickname, "anonymous")
		}
		if ada.CreatedAt.IsZero() || ada.UpdatedAt.IsZero() {
			t.Error("timestamps should be set after Insert")
		}

		got, err := rio.Find[pgUser](ctx, db, ada.ID)
		if err != nil {
			t.Fatalf("Find: %v", err)
		}
		if got.Email != ada.Email || got.Nickname != ada.Nickname {
			t.Errorf("reload = %+v, want the inserted row", got)
		}
		if !got.CreatedAt.Equal(ada.CreatedAt) {
			t.Errorf("reloaded CreatedAt %v differs from backfilled %v", got.CreatedAt, ada.CreatedAt)
		}
	})

	t.Run("DuplicateKey", func(t *testing.T) {
		dup := pgUser{Email: "ada@example.com"}
		err := rio.Insert(ctx, db, &dup)
		if !errors.Is(err, rio.ErrDuplicateKey) {
			t.Fatalf("err = %v, want rio.ErrDuplicateKey", err)
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			t.Fatalf("the *pgconn.PgError should stay in the chain, got %v", err)
		}
		if pgErr.Code != "23505" || pgErr.ConstraintName == "" {
			t.Errorf("PgError = code %s constraint %q, want 23505 with a constraint name", pgErr.Code, pgErr.ConstraintName)
		}
	})

	t.Run("ForeignKeyViolated", func(t *testing.T) {
		orphan := pgPost{UserID: ada.ID + 1_000_000, Title: "orphan"}
		err := rio.Insert(ctx, db, &orphan)
		if !errors.Is(err, rio.ErrForeignKeyViolated) {
			t.Fatalf("err = %v, want rio.ErrForeignKeyViolated", err)
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
			t.Errorf("errors.As should reach the *pgconn.PgError with code 23503, got %v", err)
		}
	})

	t.Run("SavepointNestedTx", func(t *testing.T) {
		err := db.Tx(ctx, func(tx *rio.Tx) error {
			grace := pgUser{Email: "grace@example.com"}
			if err := rio.Insert(ctx, tx, &grace); err != nil {
				return err
			}

			// The inner transaction is a savepoint. Its failed statement
			// aborts only the savepoint; ROLLBACK TO must leave the outer
			// transaction alive.
			inner := tx.Tx(ctx, func(tx2 *rio.Tx) error {
				dup := pgUser{Email: "ada@example.com"}
				return rio.Insert(ctx, tx2, &dup)
			})
			if !errors.Is(inner, rio.ErrDuplicateKey) {
				t.Errorf("inner err = %v, want rio.ErrDuplicateKey", inner)
			}

			// PostgreSQL aborts the whole transaction on error unless the
			// savepoint machinery worked; this insert proves it did.
			linus := pgUser{Email: "linus@example.com"}
			return rio.Insert(ctx, tx, &linus)
		})
		if err != nil {
			t.Fatalf("outer transaction should commit, got %v", err)
		}

		n, err := rio.From[pgUser]().Count(ctx, db)
		if err != nil {
			t.Fatalf("Count: %v", err)
		}
		if n != 3 { // ada, grace, linus — and no duplicate ada
			t.Errorf("user count = %d, want 3", n)
		}
		for _, email := range []string{"grace@example.com", "linus@example.com"} {
			if _, err := rio.From[pgUser]().Where("email = ?", email).Sole(ctx, db); err != nil {
				t.Errorf("user %s should have been committed: %v", email, err)
			}
		}
	})
}
