// Package postgres connects github.com/go-rio/rio to PostgreSQL through the
// pgx driver's database/sql adapter.
//
// The package is deliberately thin: it constructs a *rio.DB with the built-in
// rio.Postgres dialect, installs a precise error translator that maps
// *pgconn.PgError values onto rio's sentinel errors, and keeps the connection
// settings honest about standard_conforming_strings. All SQL grammar lives in
// the rio core; this module never shapes a query.
package postgres

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/go-rio/rio"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
)

// Open opens a PostgreSQL database via pgx's database/sql adapter and wraps
// it in a *rio.DB. The DSN is handed to pgx untouched, so both URL form
// (postgres://user:pass@host:5432/app) and keyword/value form
// (host=... user=... dbname=...) work, along with every pgx runtime
// parameter — except one.
//
// rio rewrites ? placeholders by lexing the SQL with
// standard_conforming_strings on, the server default since PostgreSQL 9.1:
// a backslash inside a '...' literal is an ordinary character. A session
// running with the setting off lexes those literals differently — backslash
// escapes again — so the server could disagree with rio about which ? are
// placeholders. Open therefore rejects a configuration that turns the
// setting off, whether spelled as a runtime parameter
// (standard_conforming_strings=off) or inside the options startup parameter
// (options=-c standard_conforming_strings=off — including one pgx inherits
// from the PGOPTIONS environment variable). An explicit on passes through,
// and when the setting is never mentioned nothing is injected: Open never
// connects, so it cannot see the server's value. If your server turns the
// setting off globally, turn it back on for rio's connections in the DSN —
// the README shows a paste-ready example.
//
// Open validates the DSN eagerly — pgx's database/sql adapter would
// otherwise surface a malformed DSN on the first query — but it does not
// connect; ping the underlying pool (db.Unwrap().PingContext) to verify
// connectivity. Pool tuning also happens on the *sql.DB returned by Unwrap —
// rio never replaces or configures the connection pool.
func Open(dsn string, opts ...rio.Option) (*rio.DB, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: open: %w", err)
	}
	if bad := nonConformingStringsSetting(cfg.RuntimeParams); bad != "" {
		return nil, errNonConformingStrings("open", bad)
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
// New performs no connection hygiene — the pool is the caller's; make sure
// its sessions run with standard_conforming_strings on (the server default
// since PostgreSQL 9.1), or rio's placeholder rewriting can disagree with
// the server's lexing (see Open).
//
// Options are applied after the translator, so rio.WithErrorTranslator in
// opts replaces this package's translation if you need to.
func New(db *sql.DB, opts ...rio.Option) *rio.DB {
	merged := make([]rio.Option, 0, len(opts)+1)
	merged = append(merged, rio.WithErrorTranslator(translate))
	merged = append(merged, opts...)
	return rio.New(db, rio.Postgres, merged...)
}

// errNonConformingStrings is the shared refusal for a configuration that
// turns standard_conforming_strings off, worded once for every constructor
// that can see the settings (Open and OpenPool).
func errNonConformingStrings(op, bad string) error {
	return fmt.Errorf("postgres: %s: the connection settings turn standard_conforming_strings off (%s), but rio rewrites ? placeholders assuming it is on — the server default since PostgreSQL 9.1, under which backslash is an ordinary character inside '...' literals — so the server would lex string literals differently from rio and the two could disagree on the placeholder count; remove the setting or set it to on", op, bad)
}

// nonConformingStringsSetting returns a description of the connection
// setting that turns standard_conforming_strings off, or "" when the
// parameters leave it alone (or explicitly on). It checks the two routes a
// pgx config can carry the setting: the runtime parameter itself — URL query
// and keyword/value DSNs both land here — and -c/--name flags inside the
// options startup parameter, which pgx also fills from the PGOPTIONS
// environment variable. Values are judged the way the server's parse_bool
// judges them; a value that spells neither true nor false is left for the
// server to refuse, loudly, at connect time.
func nonConformingStringsSetting(params map[string]string) string {
	for key, val := range params {
		switch {
		case strings.EqualFold(key, "standard_conforming_strings"):
			if pgFalse(val) {
				return "standard_conforming_strings=" + val
			}
		case strings.EqualFold(key, "options"):
			args := splitServerOptions(val)
			for i, arg := range args {
				var setting string
				switch {
				case arg == "-c" && i+1 < len(args):
					setting = args[i+1]
				case len(arg) > 2 && strings.HasPrefix(arg, "-c"):
					setting = arg[2:]
				case strings.HasPrefix(arg, "--"):
					setting = arg[2:]
				default:
					continue
				}
				name, value, ok := strings.Cut(setting, "=")
				if !ok {
					continue
				}
				// The server's ParseLongOption reads dashes in a GUC name
				// as underscores; GUC lookup ignores case.
				name = strings.ReplaceAll(name, "-", "_")
				if strings.EqualFold(name, "standard_conforming_strings") && pgFalse(value) {
					return "options: -c standard_conforming_strings=" + value
				}
			}
		}
	}
	return ""
}

// splitServerOptions splits an options startup parameter into arguments the
// way the server's pg_split_opts does: on whitespace, with a backslash
// escaping the byte after it (that is how a value keeps a literal space).
func splitServerOptions(s string) []string {
	var args []string
	var cur strings.Builder
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case escaped:
			cur.WriteByte(c)
			escaped = false
		case c == '\\':
			escaped = true
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f':
			if cur.Len() > 0 {
				args = append(args, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		args = append(args, cur.String())
	}
	return args
}

// pgFalse reports whether v spells boolean false the way the server's
// parse_bool does: a case-insensitive unique prefix of "false", "no" or
// "off", or the digit "0". A lone "o" is ambiguous between on and off, so
// parse_bool rejects it — and this function returns false for it, like for
// every value that is not a valid false spelling.
func pgFalse(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	switch {
	case v == "":
		return false
	case strings.HasPrefix("false", v), strings.HasPrefix("no", v):
		return true
	case v == "of" || v == "off" || v == "0":
		return true
	}
	return false
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
