package state

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"
)

// TableCopy describes how one table is copied from a legacy SQLite database into
// the PostgreSQL backend.
type TableCopy struct {
	// Name is the table name, identical in both backends.
	Name string
	// Columns lists every column in copy order.
	Columns []string
	// Key is the conflict target used for the upsert.
	Key []string
	// Bools lists columns stored as INTEGER 0/1 in SQLite and BOOLEAN in
	// PostgreSQL; their values are converted during the copy.
	Bools []string
}

// StateTables is the copy plan for the strong-persist store.
var StateTables = []TableCopy{
	{
		Name:    "system_config",
		Columns: []string{"id", "config_json", "version", "updated_at_ns"},
		Key:     []string{"id"},
	},
	{
		Name: "platforms",
		Columns: []string{
			"id", "name", "sticky_ttl_ns", "regex_filters_json", "region_filters_json",
			"reverse_proxy_miss_action", "allocation_policy", "updated_at_ns",
			"reverse_proxy_empty_account_behavior", "reverse_proxy_fixed_account_header",
			"passive_circuit_breaker_disabled",
		},
		Key:   []string{"id"},
		Bools: []string{"passive_circuit_breaker_disabled"},
	},
	{
		Name: "subscriptions",
		Columns: []string{
			"id", "name", "source_type", "url", "content", "update_interval_ns",
			"enabled", "ephemeral", "ephemeral_node_evict_delay_ns", "created_at_ns",
			"updated_at_ns", "incremental_alive_nodes",
		},
		Key:   []string{"id"},
		Bools: []string{"enabled", "ephemeral", "incremental_alive_nodes"},
	},
	{
		Name:    "account_header_rules",
		Columns: []string{"url_prefix", "headers_json", "updated_at_ns"},
		Key:     []string{"url_prefix"},
	},
	{
		Name: "endpoints",
		Columns: []string{
			"id", "port", "allow_management", "allow_proxy", "allow_http_forward",
			"allow_http_reverse", "allow_socks5", "created_at_ns", "updated_at_ns",
			"require_proxy_auth_info", "enabled",
		},
		Key: []string{"id"},
		Bools: []string{
			"allow_management", "allow_proxy", "allow_http_forward", "allow_http_reverse",
			"allow_socks5", "require_proxy_auth_info", "enabled",
		},
	},
}

// CacheTables is the copy plan for the weak-persist store.
var CacheTables = []TableCopy{
	{
		Name:    "nodes_static",
		Columns: []string{"hash", "raw_options_json", "created_at_ns"},
		Key:     []string{"hash"},
	},
	{
		Name: "nodes_dynamic",
		Columns: []string{
			"hash", "failure_count", "circuit_open_since", "egress_ip", "egress_region",
			"egress_updated_at_ns", "last_latency_probe_attempt_ns",
			"last_authority_latency_probe_attempt_ns", "last_egress_update_attempt_ns",
		},
		Key: []string{"hash"},
	},
	{
		Name:    "node_latency",
		Columns: []string{"node_hash", "domain", "ewma_ns", "last_updated_ns"},
		Key:     []string{"node_hash", "domain"},
	},
	{
		Name:    "leases",
		Columns: []string{"platform_id", "account", "node_hash", "egress_ip", "created_at_ns", "expiry_ns", "last_accessed_ns"},
		Key:     []string{"platform_id", "account"},
	},
	{
		Name:    "subscription_nodes",
		Columns: []string{"subscription_id", "node_hash", "tags_json", "evicted"},
		Key:     []string{"subscription_id", "node_hash"},
		Bools:   []string{"evicted"},
	},
}

// CopyResult reports what a single table copy did.
type CopyResult struct {
	Table    string
	Source   int64
	Written  int64
	Existing int64
	DryRun   bool
}

// OpenSQLiteReadOnly opens a legacy SQLite database without taking write locks.
func OpenSQLiteReadOnly(path string) (*sql.DB, error) {
	dsn := "file:" + url.PathEscape(path) + "?mode=ro&_pragma=query_only(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s read-only: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		// Fall back to a plain open: some builds reject the URI form for paths
		// containing characters the escaping turns into %XX.
		fallback, ferr := OpenDB(path)
		if ferr != nil {
			return nil, fmt.Errorf("open %s: %w", path, ferr)
		}
		return fallback, nil
	}
	return db, nil
}

// CopyTable copies every row of one table from src (SQLite) into dst
// (PostgreSQL), upserting on the table's primary key.
//
// It is safe to re-run: rows that already exist are overwritten with the source
// values, and nothing is deleted. In dry-run mode only counts are read.
func CopyTable(ctx context.Context, src, dst *sql.DB, plan TableCopy, batchSize int, dryRun bool) (CopyResult, error) {
	result := CopyResult{Table: plan.Name, DryRun: dryRun}

	if err := ctx.Err(); err != nil {
		return result, err
	}

	colList := strings.Join(plan.Columns, ", ")
	selectSQL := fmt.Sprintf("SELECT %s FROM %s", colList, plan.Name)

	if err := src.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+plan.Name).Scan(&result.Source); err != nil {
		return result, fmt.Errorf("%s: count source rows: %w", plan.Name, err)
	}
	if err := dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+plan.Name).Scan(&result.Existing); err != nil {
		return result, fmt.Errorf("%s: count target rows: %w", plan.Name, err)
	}
	if dryRun || result.Source == 0 {
		return result, nil
	}

	rows, err := src.QueryContext(ctx, selectSQL)
	if err != nil {
		return result, fmt.Errorf("%s: read source rows: %w", plan.Name, err)
	}
	defer rows.Close()

	insertSQL := buildUpsertSQL(plan)
	bools := boolColumnSet(plan)

	if batchSize <= 0 {
		batchSize = 1000
	}

	for {
		tx, err := dst.BeginTx(ctx, nil)
		if err != nil {
			return result, fmt.Errorf("%s: begin batch tx: %w", plan.Name, err)
		}
		stmt, err := tx.PrepareContext(ctx, insertSQL)
		if err != nil {
			tx.Rollback()
			return result, fmt.Errorf("%s: prepare upsert: %w", plan.Name, err)
		}

		copied := 0
		scanErr := error(nil)
		for copied < batchSize && rows.Next() {
			values := make([]any, len(plan.Columns))
			dest := make([]any, len(plan.Columns))
			for i := range values {
				dest[i] = &values[i]
			}
			if err := rows.Scan(dest...); err != nil {
				scanErr = err
				break
			}
			for i, col := range plan.Columns {
				values[i] = convertCopyValue(values[i], bools[col])
			}
			if _, err := stmt.ExecContext(ctx, values...); err != nil {
				scanErr = fmt.Errorf("insert row: %w", err)
				break
			}
			copied++
		}
		stmt.Close()
		if scanErr != nil {
			tx.Rollback()
			return result, fmt.Errorf("%s: %w", plan.Name, scanErr)
		}
		if copied == 0 {
			tx.Rollback()
			break
		}
		if err := tx.Commit(); err != nil {
			return result, fmt.Errorf("%s: commit batch: %w", plan.Name, err)
		}
		result.Written += int64(copied)
		if copied < batchSize {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return result, fmt.Errorf("%s: iterate source rows: %w", plan.Name, err)
	}
	return result, nil
}

func buildUpsertSQL(plan TableCopy) string {
	placeholders := make([]string, len(plan.Columns))
	for i := range placeholders {
		placeholders[i] = "?"
	}
	updates := make([]string, 0, len(plan.Columns))
	for _, col := range plan.Columns {
		if contains(plan.Key, col) {
			continue
		}
		updates = append(updates, fmt.Sprintf("%s = excluded.%s", col, col))
	}
	base := fmt.Sprintf(
		"INSERT INTO %s (%s) VALUES (%s)",
		plan.Name, strings.Join(plan.Columns, ", "), strings.Join(placeholders, ", "),
	)
	if len(updates) == 0 {
		return base + fmt.Sprintf(" ON CONFLICT (%s) DO NOTHING", strings.Join(plan.Key, ", "))
	}
	return base + fmt.Sprintf(
		" ON CONFLICT (%s) DO UPDATE SET %s",
		strings.Join(plan.Key, ", "), strings.Join(updates, ", "),
	)
}

func boolColumnSet(plan TableCopy) map[string]bool {
	set := make(map[string]bool, len(plan.Bools))
	for _, col := range plan.Bools {
		set[col] = true
	}
	return set
}

// convertCopyValue adapts a value read from SQLite to the PostgreSQL column type.
func convertCopyValue(value any, isBool bool) any {
	switch v := value.(type) {
	case []byte:
		if isBool {
			return len(v) > 0 && (v[0] == '1' || v[0] == 't' || v[0] == 'T')
		}
		return string(v)
	case int64:
		if isBool {
			return v != 0
		}
		return v
	case bool:
		return v
	default:
		return value
	}
}

func contains(list []string, item string) bool {
	for _, v := range list {
		if v == item {
			return true
		}
	}
	return false
}

// VerifyCopy re-reads target row counts and reports any table where the target
// holds fewer rows than the source, which would mean a silent copy failure.
func VerifyCopy(ctx context.Context, src, dst *sql.DB, plans []TableCopy) error {
	var problems []string
	for _, plan := range plans {
		var sourceRows, targetRows int64
		if err := src.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+plan.Name).Scan(&sourceRows); err != nil {
			return fmt.Errorf("%s: count source: %w", plan.Name, err)
		}
		if err := dst.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+plan.Name).Scan(&targetRows); err != nil {
			return fmt.Errorf("%s: count target: %w", plan.Name, err)
		}
		if targetRows < sourceRows {
			problems = append(problems, fmt.Sprintf("%s: source=%d target=%d", plan.Name, sourceRows, targetRows))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("copy verification failed: %s", strings.Join(problems, "; "))
	}
	return nil
}
