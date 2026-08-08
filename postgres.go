// Package postgres connects rio to PostgreSQL through pgx's database/sql
// adapter or rio's native execution path.
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

// Open validates a pgx DSN and returns a rio DB using pgx's database/sql
// adapter. It does not connect; use db.Unwrap().PingContext to verify
// connectivity and db.Unwrap() to configure the pool.
//
// Open rejects standard_conforming_strings=off, including settings supplied
// through PGOPTIONS, because rio's placeholder lexer assumes the PostgreSQL
// default. URL and keyword/value DSNs are both accepted.
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

// New wraps an existing *sql.DB with the Postgres dialect and error
// translator. The caller must ensure standard_conforming_strings is on.
// Options are applied last, so a supplied translator replaces the default.
func New(db *sql.DB, opts ...rio.Option) *rio.DB {
	merged := make([]rio.Option, 0, len(opts)+1)
	merged = append(merged, rio.WithErrorTranslator(translate))
	merged = append(merged, opts...)
	return rio.New(db, rio.Postgres, merged...)
}

func errNonConformingStrings(op, bad string) error {
	return fmt.Errorf(
		"postgres: %s: the connection settings turn standard_conforming_strings off (%s), "+
			"but rio rewrites ? placeholders assuming it is on — the server default since PostgreSQL 9.1, "+
			"under which backslash is an ordinary character inside '...' literals — "+
			"so the server would lex string literals differently from rio and "+
			"the two could disagree on the placeholder count; remove the setting or set it to on",
		op,
		bad,
	)
}

// nonConformingStringsSetting finds a false standard_conforming_strings
// runtime parameter or startup option. Invalid boolean values are left for
// PostgreSQL to reject.
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

// splitServerOptions follows PostgreSQL's whitespace and backslash rules for
// startup options.
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

// pgFalse accepts PostgreSQL's unambiguous false spellings and prefixes.
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

// PostgreSQL integrity-constraint SQLSTATE codes translated by this package.
const (
	codeUniqueViolation     = "23505"
	codeForeignKeyViolation = "23503"
)

// translate maps recognized pgx errors to rio sentinels.
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
