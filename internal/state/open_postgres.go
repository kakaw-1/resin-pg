package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	pgxstdlib "github.com/jackc/pgx/v5/stdlib"
)

// errNamedArgsUnsupported is returned when a caller uses driver named parameters,
// which the placeholder-rewriting adapter cannot map.
var errNamedArgsUnsupported = errors.New("state: driver named parameters are not supported")

// openPostgres opens a pooled PostgreSQL handle whose connections start with
// search_path set to schema (empty means "leave the server default alone").
//
// The pool is wrapped by the placeholder-rebinding connector so that Resin's
// '?'-style SQL runs unmodified.
func openPostgres(cfg DBConfig, schema string) (*sql.DB, error) {
	pgCfg, err := pgx.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse db dsn: %w", err)
	}
	if pgCfg.RuntimeParams == nil {
		pgCfg.RuntimeParams = make(map[string]string, 1)
	}
	if schema != "" {
		// search_path is a legal startup parameter; every pooled connection
		// inherits it, which is what makes unqualified table names in the repos
		// resolve to the state or cache schema.
		pgCfg.RuntimeParams["search_path"] = schema
	}
	if cfg.ConnectTimeout > 0 {
		pgCfg.ConnectTimeout = cfg.ConnectTimeout
	}

	db := sql.OpenDB(newRebindConnector(pgxstdlib.GetConnector(*pgCfg)))
	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	db.SetConnMaxIdleTime(cfg.ConnMaxLifetime / 2)

	timeout := cfg.ConnectTimeout
	if timeout <= 0 {
		timeout = defaultPGConnectTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return db, nil
}

// ensurePostgresSchemas creates both namespaces when they do not exist yet.
// It runs on a connection that is not bound to either schema.
func ensurePostgresSchemas(cfg DBConfig) error {
	admin, err := openPostgres(cfg, "")
	if err != nil {
		return err
	}
	defer admin.Close()

	for _, schema := range []string{cfg.StateSchema, cfg.CacheSchema} {
		if _, err := admin.Exec("CREATE SCHEMA IF NOT EXISTS " + quoteIdentifier(schema)); err != nil {
			return fmt.Errorf("create schema %s: %w", schema, err)
		}
	}
	return nil
}

// RedactDSN removes credentials from a DSN so it can be logged or returned by the
// system info endpoint.
func RedactDSN(dsn string) string { return redactDSN(dsn) }

// redactDSN removes credentials from a DSN so it can be logged.
func redactDSN(dsn string) string {
	if dsn == "" {
		return ""
	}
	// URL form: postgres://user:password@host/db
	if i := strings.Index(dsn, "://"); i >= 0 {
		scheme, rest := dsn[:i+3], dsn[i+3:]
		at := strings.LastIndex(rest, "@")
		if at < 0 {
			return dsn
		}
		cred := rest[:at]
		if colon := strings.Index(cred, ":"); colon >= 0 {
			cred = cred[:colon] + ":***"
		}
		return scheme + cred + rest[at:]
	}
	// Keyword form: host=... user=... password=...
	fields := strings.Fields(dsn)
	for i, f := range fields {
		if strings.HasPrefix(strings.ToLower(f), "password=") {
			fields[i] = "password=***"
		}
	}
	return strings.Join(fields, " ")
}

// describeDB returns a one-line, credential-free description for startup logs
// and the system info endpoint.
func describeDB(cfg DBConfig) string {
	if cfg.Driver != DriverPostgres {
		return "sqlite"
	}
	return fmt.Sprintf("postgres (%s, schema %s/%s)", redactDSN(cfg.DSN), cfg.StateSchema, cfg.CacheSchema)
}

// isUniqueViolation reports whether err is a uniqueness violation on any
// supported driver, so repo callers can translate it into ErrConflict.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return isSQLiteUniqueConstraint(err) || isPostgresUniqueViolation(err)
}

// isRetryableTxError reports transient transaction conflicts worth retrying:
// serialization failure (40001) and deadlock detected (40P01).
func isRetryableTxError(err error) bool {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		switch pgErr.SQLState() {
		case "40001", "40P01":
			return true
		}
	}
	return false
}

// sleepBeforeRetry backs off between transaction retries.
func sleepBeforeRetry(attempt int) {
	d := time.Duration(attempt) * 20 * time.Millisecond
	time.Sleep(d)
}

// isPostgresUniqueViolation reports SQLSTATE 23505.
func isPostgresUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}
