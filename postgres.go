// Package postgres connects github.com/go-rio/rio to PostgreSQL through the
// pgx driver's database/sql adapter.
//
// The package is deliberately thin: it constructs a *rio.DB with the built-in
// rio.Postgres dialect and installs a precise error translator that maps
// *pgconn.PgError values onto rio's sentinel errors. All SQL grammar lives in
// the rio core; this module never shapes a query.
package postgres

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/go-rio/rio"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
)

// Open opens a PostgreSQL database via pgx's database/sql adapter and wraps
// it in a *rio.DB. The DSN is handed to pgx untouched, so both URL form
// (postgres://user:pass@host:5432/app) and keyword/value form
// (host=... user=... dbname=...) work, along with every pgx runtime
// parameter.
//
// Open validates the DSN eagerly — pgx's database/sql adapter would
// otherwise surface a malformed DSN on the first query — but it does not
// connect; ping the underlying pool (db.Unwrap().PingContext) to verify
// connectivity. Pool tuning also happens on the *sql.DB returned by Unwrap —
// rio never replaces or configures the connection pool.
func Open(dsn string, opts ...rio.Option) (*rio.DB, error) {
	if _, err := pgx.ParseConfig(dsn); err != nil {
		return nil, fmt.Errorf("postgres: open: %w", err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: open: %w", err)
	}
	return New(db, opts...), nil
}

// New wraps an existing *sql.DB in a *rio.DB with the Postgres dialect and
// this package's error translator. Use it when you bring your own pool: a
// *sql.DB you tuned yourself, or one derived from a pgxpool.Pool via
// stdlib.OpenDBFromPool.
//
// Options are applied after the translator, so rio.WithErrorTranslator in
// opts replaces this package's translation if you need to.
func New(db *sql.DB, opts ...rio.Option) *rio.DB {
	merged := make([]rio.Option, 0, len(opts)+1)
	merged = append(merged, rio.WithErrorTranslator(translate))
	merged = append(merged, opts...)
	return rio.New(db, rio.Postgres, merged...)
}

// PostgreSQL error codes translated by this package. Class 23 is "Integrity
// Constraint Violation" in the SQLSTATE standard.
const (
	codeUniqueViolation     = "23505"
	codeForeignKeyViolation = "23503"
)

// translate maps a pgx error to the matching rio sentinel, or returns nil
// when the error is not one this package recognizes. rio keeps the original
// error in the chain, so errors.As still reaches the *pgconn.PgError with
// the constraint name, table, and detail intact.
func translate(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return nil
	}
	switch pgErr.Code {
	case codeUniqueViolation:
		return rio.ErrDuplicateKey
	case codeForeignKeyViolation:
		return rio.ErrForeignKeyViolated
	}
	return nil
}
