package state

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
)

// BootstrapConfig selects and locates the persistence backend.
//
// StateDir/CacheDir are used by the SQLite backend (the default). DB selects the
// backend explicitly; when DB.Driver is postgres the two directories are unused
// for state/cache because every strong- and weak-persist row lives in the
// external database. Metrics and request logs always stay on local files.
type BootstrapConfig struct {
	StateDir string
	CacheDir string
	DB       DBConfig
}

// persistenceCloser holds DB handles for cleanup. Implements io.Closer.
type persistenceCloser struct {
	stateDB *sql.DB
	cacheDB *sql.DB
}

func (c *persistenceCloser) Close() error {
	return errors.Join(c.stateDB.Close(), c.cacheDB.Close())
}

// PersistenceBootstrap initializes both databases, runs consistency repair,
// and returns a ready-to-use StateEngine plus an io.Closer for the DB handles.
// It is the SQLite entry point retained for existing callers and tests.
func PersistenceBootstrap(stateDir, cacheDir string) (engine *StateEngine, closer io.Closer, err error) {
	return PersistenceBootstrapWith(BootstrapConfig{
		StateDir: stateDir,
		CacheDir: cacheDir,
		DB:       DBConfig{Driver: DriverSQLite},
	})
}

// PersistenceBootstrapWith initializes the configured backend.
//
// Steps:
//  1. Open/create the state and cache stores with the right driver and options.
//  2. Run schema migrations on both stores.
//  3. Run consistency repair (cross-store orphan cleanup).
//  4. Construct and return StateEngine.
func PersistenceBootstrapWith(cfg BootstrapConfig) (engine *StateEngine, closer io.Closer, err error) {
	dbCfg := cfg.DB.WithDefaults()
	if err := dbCfg.Validate(); err != nil {
		return nil, nil, err
	}

	if dbCfg.Driver == DriverPostgres {
		return bootstrapPostgres(dbCfg)
	}
	return bootstrapSQLite(cfg.StateDir, cfg.CacheDir)
}

func bootstrapSQLite(stateDir, cacheDir string) (engine *StateEngine, closer io.Closer, err error) {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("create state dir %s: %w", stateDir, err)
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("create cache dir %s: %w", cacheDir, err)
	}

	stateDBPath := filepath.Join(stateDir, "state.db")
	cacheDBPath := filepath.Join(cacheDir, "cache.db")

	stateDB, err := OpenDB(stateDBPath)
	if err != nil {
		return nil, nil, fmt.Errorf("open state.db: %w", err)
	}

	cacheDB, err := OpenDB(cacheDBPath)
	if err != nil {
		stateDB.Close()
		return nil, nil, fmt.Errorf("open cache.db: %w", err)
	}

	if err := MigrateStateDB(stateDB); err != nil {
		stateDB.Close()
		cacheDB.Close()
		return nil, nil, fmt.Errorf("migrate state.db: %w", err)
	}

	if err := MigrateCacheDB(cacheDB); err != nil {
		stateDB.Close()
		cacheDB.Close()
		return nil, nil, fmt.Errorf("migrate cache.db: %w", err)
	}

	if err := RepairConsistency(stateDBPath, cacheDB); err != nil {
		stateDB.Close()
		cacheDB.Close()
		return nil, nil, fmt.Errorf("repair consistency: %w", err)
	}

	stateRepo := newStateRepo(stateDB)
	cacheRepo := newCacheRepo(cacheDB)
	engine = newStateEngine(stateRepo, cacheRepo)

	return engine, &persistenceCloser{stateDB: stateDB, cacheDB: cacheDB}, nil
}

// bootstrapPostgres opens two schemas of one database, migrates both, then runs
// the cross-store consistency repair.
func bootstrapPostgres(dbCfg DBConfig) (engine *StateEngine, closer io.Closer, err error) {
	log.Printf("[state] persistence backend: %s", describeDB(dbCfg))

	if err := ensurePostgresSchemas(dbCfg); err != nil {
		return nil, nil, fmt.Errorf("prepare postgres schemas: %w", err)
	}

	stateDB, err := openPostgres(dbCfg, dbCfg.StateSchema)
	if err != nil {
		return nil, nil, fmt.Errorf("open state schema %s: %w", dbCfg.StateSchema, err)
	}

	cacheDB, err := openPostgres(dbCfg, dbCfg.CacheSchema)
	if err != nil {
		stateDB.Close()
		return nil, nil, fmt.Errorf("open cache schema %s: %w", dbCfg.CacheSchema, err)
	}

	if err := migrateStateDBFor(stateDB, DriverPostgres, dbCfg.StateSchema); err != nil {
		stateDB.Close()
		cacheDB.Close()
		return nil, nil, fmt.Errorf("migrate state schema: %w", err)
	}

	if err := migrateCacheDBFor(cacheDB, DriverPostgres, dbCfg.CacheSchema); err != nil {
		stateDB.Close()
		cacheDB.Close()
		return nil, nil, fmt.Errorf("migrate cache schema: %w", err)
	}

	if err := repairConsistencyPostgres(cacheDB, dbCfg.StateSchema); err != nil {
		stateDB.Close()
		cacheDB.Close()
		return nil, nil, fmt.Errorf("repair consistency: %w", err)
	}

	stateRepo := newStateRepo(stateDB)
	cacheRepo := newCacheRepo(cacheDB)
	engine = newStateEngine(stateRepo, cacheRepo)

	return engine, &persistenceCloser{stateDB: stateDB, cacheDB: cacheDB}, nil
}
