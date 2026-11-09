package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Resinat/Resin/internal/state"
)

// runDBMigrate copies a legacy SQLite state/cache store into the external
// PostgreSQL backend selected by the same RESIN_DB_* settings the server uses.
//
// The command is additive and idempotent: it never deletes target rows, and
// re-running it overwrites the copied tables with the source values. The target
// schemas are migrated first, so a brand-new database works too.
func runDBMigrate(args []string) error {
	fs := flag.NewFlagSet("db-migrate", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	fromState := fs.String("from-state", defaultLegacyPath("RESIN_STATE_DIR", "/var/lib/resin", "state.db"), "path to the legacy state.db")
	fromCache := fs.String("from-cache", defaultLegacyPath("RESIN_CACHE_DIR", "/var/cache/resin", "cache.db"), "path to the legacy cache.db")
	dsn := fs.String("dsn", "", "target PostgreSQL DSN (defaults to RESIN_DB_DSN)")
	stateSchema := fs.String("state-schema", envOr("RESIN_DB_STATE_SCHEMA", "resin_state"), "target schema for state data")
	cacheSchema := fs.String("cache-schema", envOr("RESIN_DB_CACHE_SCHEMA", "resin_cache"), "target schema for cache data")
	batchSize := fs.Int("batch-size", 1000, "rows per transaction")
	dryRun := fs.Bool("dry-run", false, "only report row counts, write nothing")
	skipVerify := fs.Bool("skip-verify", false, "skip the post-copy row-count check")

	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: resin db-migrate [flags]

Copies the SQLite state/cache stores into PostgreSQL. Metrics and request logs
are not part of this migration: they always stay on local files.

Flags:
`)
		fs.PrintDefaults()
		fmt.Fprintf(os.Stderr, `
Example:
  RESIN_DB_DSN='postgres://resin:secret@db:5432/resin?sslmode=disable' \
    resin db-migrate --from-state /var/lib/resin/state.db \
                     --from-cache /var/cache/resin/cache.db --dry-run
`)
	}

	if err := fs.Parse(args); err != nil {
		return err
	}

	if *dsn == "" {
		*dsn = strings.TrimSpace(os.Getenv("RESIN_DB_DSN"))
	}
	if *dsn == "" {
		return fmt.Errorf("db-migrate: no target DSN (pass --dsn or set RESIN_DB_DSN)")
	}

	cfg := state.DBConfig{
		Driver:          state.DriverPostgres,
		DSN:             *dsn,
		StateSchema:     *stateSchema,
		CacheSchema:     *cacheSchema,
		MaxOpenConns:    4,
		MaxIdleConns:    2,
		ConnMaxLifetime: 5 * time.Minute,
		ConnectTimeout:  15 * time.Second,
	}

	log.Printf("[db-migrate] target: postgres %s (state=%s cache=%s)",
		state.RedactDSN(cfg.DSN), cfg.StateSchema, cfg.CacheSchema)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()

	// Bootstrap the target first: schemas + migrations must exist before any copy.
	engine, closer, err := state.PersistenceBootstrapWith(state.BootstrapConfig{DB: cfg})
	if err != nil {
		return fmt.Errorf("prepare target database: %w", err)
	}
	stateDst := engine.StateRepo.DB()
	cacheDst := engine.CacheRepo.DB()

	srcState, err := state.OpenSQLiteReadOnly(*fromState)
	if err != nil {
		closer.Close()
		return fmt.Errorf("open legacy state store: %w", err)
	}
	defer srcState.Close()

	srcCache, err := state.OpenSQLiteReadOnly(*fromCache)
	if err != nil {
		closer.Close()
		return fmt.Errorf("open legacy cache store: %w", err)
	}
	defer srcCache.Close()

	var results []state.CopyResult

	for _, plan := range state.StateTables {
		res, err := state.CopyTable(ctx, srcState, stateDst, plan, *batchSize, *dryRun)
		if err != nil {
			closer.Close()
			return err
		}
		results = append(results, res)
	}
	for _, plan := range state.CacheTables {
		res, err := state.CopyTable(ctx, srcCache, cacheDst, plan, *batchSize, *dryRun)
		if err != nil {
			closer.Close()
			return err
		}
		results = append(results, res)
	}

	logCopySummary(results)

	if *dryRun {
		closer.Close()
		log.Printf("[db-migrate] dry run: nothing was written")
		return nil
	}

	if !*skipVerify {
		if err := state.VerifyCopy(ctx, srcState, stateDst, state.StateTables); err != nil {
			closer.Close()
			return err
		}
		if err := state.VerifyCopy(ctx, srcCache, cacheDst, state.CacheTables); err != nil {
			closer.Close()
			return err
		}
		log.Printf("[db-migrate] row counts verified for %d tables", len(state.StateTables)+len(state.CacheTables))
	}

	// Re-bootstrap so the cross-store orphan cleanup runs against the copied data.
	closer.Close()
	if _, closer2, err := state.PersistenceBootstrapWith(state.BootstrapConfig{DB: cfg}); err != nil {
		return fmt.Errorf("post-copy consistency repair: %w", err)
	} else {
		closer2.Close()
	}

	log.Printf("[db-migrate] done: state and cache copied into postgres schemas %s/%s",
		cfg.StateSchema, cfg.CacheSchema)
	return nil
}

func logCopySummary(results []state.CopyResult) {
	for _, r := range results {
		mode := "copied"
		if r.DryRun {
			mode = "dry-run"
		}
		log.Printf("[db-migrate] %-20s source=%-8d written=%-8d target_before=%-8d (%s)",
			r.Table, r.Source, r.Written, r.Existing, mode)
	}
}

func defaultLegacyPath(envKey, dirDefault, file string) string {
	return filepath.Join(envOr(envKey, dirDefault), file)
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
