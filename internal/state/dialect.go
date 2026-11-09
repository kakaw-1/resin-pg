package state

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Driver identifies which SQL dialect backs the state/cache stores.
type Driver string

const (
	// DriverSQLite is the built-in, zero-configuration backend (default).
	DriverSQLite Driver = "sqlite"
	// DriverPostgres stores state and cache in an external PostgreSQL server.
	DriverPostgres Driver = "postgres"
)

// DBConfig describes where and how the strong-persist (state) and weak-persist
// (cache) stores are opened.
//
// SQLite mode uses StateDir/CacheDir. PostgreSQL mode uses DSN plus two schemas
// living in the same database: the cross-store consistency repair of Resin
// relies on joining both schemas, which SQLite expresses with ATTACH.
type DBConfig struct {
	Driver Driver
	// DSN is a libpq/pgx connection string or URL, used only for DriverPostgres.
	DSN string

	// StateSchema and CacheSchema must both live in the database addressed by DSN.
	StateSchema string
	CacheSchema string

	// Connection pool knobs. Zero values fall back to per-driver defaults.
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnectTimeout  time.Duration
}

// Defaults for the PostgreSQL pool. The dirty-set flush worker is the only
// writer to the cache schema, but reads (UI, API, probes) are concurrent.
const (
	defaultPGMaxOpenConns    = 8
	defaultPGMaxIdleConns    = 4
	defaultPGConnMaxLifetime = 30 * time.Minute
	defaultPGConnectTimeout  = 10 * time.Second

	defaultStateSchema = "resin_state"
	defaultCacheSchema = "resin_cache"
)

var schemaIdentifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}$`)

// WithDefaults returns a copy of cfg with driver defaults applied and
// identifiers normalized.
func (cfg DBConfig) WithDefaults() DBConfig {
	if cfg.Driver == "" {
		cfg.Driver = DriverSQLite
	}
	cfg.Driver = Driver(strings.ToLower(strings.TrimSpace(string(cfg.Driver))))
	cfg.DSN = strings.TrimSpace(cfg.DSN)
	if cfg.StateSchema == "" {
		cfg.StateSchema = defaultStateSchema
	}
	if cfg.CacheSchema == "" {
		cfg.CacheSchema = defaultCacheSchema
	}
	cfg.StateSchema = strings.TrimSpace(cfg.StateSchema)
	cfg.CacheSchema = strings.TrimSpace(cfg.CacheSchema)

	if cfg.Driver == DriverPostgres {
		if cfg.MaxOpenConns <= 0 {
			cfg.MaxOpenConns = defaultPGMaxOpenConns
		}
		if cfg.MaxIdleConns <= 0 {
			cfg.MaxIdleConns = defaultPGMaxIdleConns
		}
		if cfg.ConnMaxLifetime <= 0 {
			cfg.ConnMaxLifetime = defaultPGConnMaxLifetime
		}
		if cfg.ConnectTimeout <= 0 {
			cfg.ConnectTimeout = defaultPGConnectTimeout
		}
	} else if cfg.MaxOpenConns <= 0 {
		// SQLite is single-writer: one connection is the correct pool size.
		cfg.MaxOpenConns = 1
	}
	if cfg.MaxIdleConns > cfg.MaxOpenConns {
		cfg.MaxIdleConns = cfg.MaxOpenConns
	}
	return cfg
}

// Validate reports configuration errors that would otherwise surface much later
// as opaque driver failures.
func (cfg DBConfig) Validate() error {
	switch cfg.Driver {
	case DriverSQLite:
		return nil
	case DriverPostgres:
		if cfg.DSN == "" {
			return fmt.Errorf("db: driver=postgres requires a DSN (RESIN_DB_DSN)")
		}
		if err := validateSchemaIdentifier("state", cfg.StateSchema); err != nil {
			return err
		}
		if err := validateSchemaIdentifier("cache", cfg.CacheSchema); err != nil {
			return err
		}
		if cfg.StateSchema == cfg.CacheSchema {
			return fmt.Errorf(
				"db: state and cache schemas must differ (both %q); cross-store consistency repair needs two namespaces",
				cfg.StateSchema,
			)
		}
		return nil
	default:
		return fmt.Errorf("db: unsupported driver %q (supported: %s, %s)", cfg.Driver, DriverSQLite, DriverPostgres)
	}
}

func validateSchemaIdentifier(role, name string) error {
	if !schemaIdentifierPattern.MatchString(name) {
		return fmt.Errorf(
			"db: %s schema %q is not a valid identifier (letters, digits, underscore; must not start with a digit)",
			role, name,
		)
	}
	return nil
}

// quoteIdentifier quotes a validated identifier for SQL text.
func quoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
