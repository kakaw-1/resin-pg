package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/config"
	"github.com/Resinat/Resin/internal/model"
)

// PostgreSQL integration tests run against a real server. Set RESIN_TEST_PG_DSN to
// enable them, e.g.
//
//	RESIN_TEST_PG_DSN='postgres://resin:pw@127.0.0.1:5432/resin_test?sslmode=disable' go test ./internal/state/
//
// Every test uses its own pair of schemas so runs never collide.
func pgTestConfig(t *testing.T) DBConfig {
	t.Helper()
	dsn := os.Getenv("RESIN_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("RESIN_TEST_PG_DSN not set")
	}
	suffix := time.Now().UnixNano()
	return DBConfig{
		Driver:          DriverPostgres,
		DSN:             dsn,
		StateSchema:     fmt.Sprintf("resin_it_state_%d", suffix),
		CacheSchema:     fmt.Sprintf("resin_it_cache_%d", suffix),
		MaxOpenConns:    4,
		MaxIdleConns:    2,
		ConnMaxLifetime: time.Minute,
		ConnectTimeout:  10 * time.Second,
	}
}

func newPGEngine(t *testing.T, cfg DBConfig) *StateEngine {
	t.Helper()
	engine, closer, err := PersistenceBootstrapWith(BootstrapConfig{DB: cfg})
	if err != nil {
		t.Fatalf("bootstrap postgres: %v", err)
	}
	t.Cleanup(func() {
		closer.Close()
		dropPGSchemas(t, cfg)
	})
	return engine
}

func dropPGSchemas(t *testing.T, cfg DBConfig) {
	t.Helper()
	cfg.MaxOpenConns = 1
	admin, err := openPostgres(cfg, "")
	if err != nil {
		t.Logf("cleanup: open admin: %v", err)
		return
	}
	defer admin.Close()
	for _, schema := range []string{cfg.StateSchema, cfg.CacheSchema} {
		if _, err := admin.Exec("DROP SCHEMA IF EXISTS " + quoteIdentifier(schema) + " CASCADE"); err != nil {
			t.Logf("cleanup: drop schema %s: %v", schema, err)
		}
	}
}

func TestSQLRebind(t *testing.T) {
	cases := []struct{ in, want string }{
		{"SELECT 1", "SELECT 1"},
		{"SELECT * FROM t WHERE a = ? AND b = ?", "SELECT * FROM t WHERE a = $1 AND b = $2"},
		{"INSERT INTO t (a) VALUES (?) ON CONFLICT(a) DO UPDATE SET a = excluded.a", "INSERT INTO t (a) VALUES ($1) ON CONFLICT(a) DO UPDATE SET a = excluded.a"},
		{"SELECT '?' AS q, ? AS v", "SELECT '?' AS q, $1 AS v"},
		{"SELECT 'it''s ?' , ?", "SELECT 'it''s ?' , $1"},
		{`SELECT "we?ird", ? FROM t`, `SELECT "we?ird", $1 FROM t`},
		{"SELECT 1 -- what?\n, ?", "SELECT 1 -- what?\n, $1"},
		{"SELECT /* ? */ ?", "SELECT /* ? */ $1"},
		{"SELECT $$ a? $$, ?", "SELECT $$ a? $$, $1"},
		{"SELECT $tag$ ? $tag$, ?", "SELECT $tag$ ? $tag$, $1"},
		{"SELECT $1 FROM t WHERE a = $1", "SELECT $1 FROM t WHERE a = $1"},
	}
	for _, c := range cases {
		if got := SQLRebind(c.in); got != c.want {
			t.Errorf("SQLRebind(%q)\n got: %q\nwant: %q", c.in, got, c.want)
		}
	}
}

func TestPostgres_BootstrapMigratesBothSchemas(t *testing.T) {
	cfg := pgTestConfig(t)
	engine := newPGEngine(t, cfg)

	for _, tc := range []struct {
		db     *sql.DB
		schema string
		want   int
	}{
		{engine.StateRepo.DB(), cfg.StateSchema, stateLatestVersion},
		{engine.CacheRepo.DB(), cfg.CacheSchema, 2},
	} {
		var version int
		var dirty bool
		q := fmt.Sprintf("SELECT version, dirty FROM %s.schema_migrations", quoteIdentifier(tc.schema))
		if err := tc.db.QueryRow(q).Scan(&version, &dirty); err != nil {
			t.Fatalf("read migration version for %s: %v", tc.schema, err)
		}
		if version != tc.want || dirty {
			t.Fatalf("%s: version=%d dirty=%v, want %d/false", tc.schema, version, dirty, tc.want)
		}
	}
}

func TestPostgres_StrongPersistRoundTrip(t *testing.T) {
	cfg := pgTestConfig(t)
	engine := newPGEngine(t, cfg)

	// system_config
	wantCfg := config.NewDefaultRuntimeConfig()
	wantCfg.MaxConsecutiveFailures = 7
	if err := engine.SaveSystemConfig(wantCfg, 3, 1234567890); err != nil {
		t.Fatalf("save system config: %v", err)
	}
	gotCfg, version, err := engine.GetSystemConfig()
	if err != nil {
		t.Fatalf("get system config: %v", err)
	}
	if version != 3 || gotCfg.MaxConsecutiveFailures != 7 {
		t.Fatalf("system config round trip: version=%d max_failures=%d", version, gotCfg.MaxConsecutiveFailures)
	}

	// platforms, including the boolean column and the unique-name conflict.
	platform := model.Platform{
		ID: "p1", Name: "PlatformOne", StickyTTLNs: int64(7 * 24 * time.Hour),
		RegexFilters:                     []string{"*example.com", `\!bad`},
		RegionFilters:                    []string{"us"},
		AllocationPolicy:                 "BALANCED",
		ReverseProxyMissAction:           "TREAT_AS_EMPTY",
		ReverseProxyEmptyAccountBehavior: "RANDOM",
		PassiveCircuitBreakerDisabled:    true,
		UpdatedAtNs:                      11,
	}
	if err := engine.UpsertPlatform(platform); err != nil {
		t.Fatalf("upsert platform: %v", err)
	}
	loaded, err := engine.GetPlatform("p1")
	if err != nil {
		t.Fatalf("get platform: %v", err)
	}
	if !loaded.PassiveCircuitBreakerDisabled {
		t.Fatalf("boolean column did not survive: %+v", loaded)
	}
	if len(loaded.RegexFilters) != 2 || loaded.RegexFilters[1] != `\!bad` {
		t.Fatalf("regex filters round trip: %+v", loaded.RegexFilters)
	}

	dup := platform
	dup.ID = "p2"
	if err := engine.UpsertPlatform(dup); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate platform name: got %v, want ErrConflict", err)
	}

	// subscriptions (three boolean columns)
	sub := model.Subscription{
		ID: "s1", Name: "Sub", URL: "https://example.com/sub",
		UpdateIntervalNs: int64(time.Hour), Enabled: true, Ephemeral: true,
		IncrementalAliveNodes: true, EphemeralNodeEvictDelayNs: int64(time.Hour),
		CreatedAtNs: 1, UpdatedAtNs: 2,
	}
	if err := engine.UpsertSubscription(sub); err != nil {
		t.Fatalf("upsert subscription: %v", err)
	}
	subs, err := engine.ListSubscriptions()
	if err != nil || len(subs) != 1 {
		t.Fatalf("list subscriptions: %v (n=%d)", err, len(subs))
	}
	if !subs[0].Enabled || !subs[0].Ephemeral || !subs[0].IncrementalAliveNodes {
		t.Fatalf("subscription booleans lost: %+v", subs[0])
	}

	// endpoints (seven boolean columns + unique port conflict)
	endpoint := model.Endpoint{
		ID: "e1", Port: 12345, Enabled: true, AllowManagement: true, AllowProxy: true,
		RequireProxyAuthInfo: true, AllowHTTPForward: true, AllowHTTPReverse: false,
		AllowSOCKS5: true, CreatedAtNs: 5, UpdatedAtNs: 6,
	}
	if err := engine.InsertEndpoint(endpoint); err != nil {
		t.Fatalf("insert endpoint: %v", err)
	}
	gotEndpoint, err := engine.GetEndpoint("e1")
	if err != nil {
		t.Fatalf("get endpoint: %v", err)
	}
	if !gotEndpoint.Enabled || !gotEndpoint.RequireProxyAuthInfo || gotEndpoint.AllowHTTPReverse {
		t.Fatalf("endpoint booleans lost: %+v", gotEndpoint)
	}
	conflict := endpoint
	conflict.ID = "e2"
	if err := engine.InsertEndpoint(conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate endpoint port: got %v, want ErrConflict", err)
	}

	// account header rules
	created, err := engine.EnsureAccountHeaderRule(model.AccountHeaderRule{
		URLPrefix: "https://api.example.com", Headers: []string{"Authorization"}, UpdatedAtNs: 7,
	})
	if err != nil || !created {
		t.Fatalf("ensure header rule: created=%v err=%v", created, err)
	}
	created, err = engine.EnsureAccountHeaderRule(model.AccountHeaderRule{
		URLPrefix: "https://api.example.com", Headers: []string{"X-Key"}, UpdatedAtNs: 8,
	})
	if err != nil || created {
		t.Fatalf("ensure existing header rule: created=%v err=%v", created, err)
	}
}

func TestPostgres_DirtySetFlush(t *testing.T) {
	cfg := pgTestConfig(t)
	engine := newPGEngine(t, cfg)

	now := time.Now().UnixNano()
	engine.MarkNodeStatic("h1")
	engine.MarkNodeDynamic("h1")
	engine.MarkNodeLatency("h1", "example.com")
	engine.MarkLease("p1", "acct-1")
	engine.MarkSubscriptionNode("s1", "h1")
	engine.MarkNodeStaticDelete("gone")

	readers := CacheReaders{
		ReadNodeStatic: func(hash string) *model.NodeStatic {
			if hash != "h1" {
				return nil
			}
			return &model.NodeStatic{Hash: hash, RawOptions: []byte(`{"type":"socks","server":"1.2.3.4"}`), CreatedAtNs: now}
		},
		ReadNodeDynamic: func(hash string) *model.NodeDynamic {
			return &model.NodeDynamic{Hash: hash, FailureCount: 2, EgressIP: "9.9.9.9", EgressRegion: "US", EgressUpdatedAtNs: now}
		},
		ReadNodeLatency: func(key NodeLatencyDirtyKey) *model.NodeLatency {
			return &model.NodeLatency{NodeHash: key.NodeHash, Domain: key.Domain, EwmaNs: 1_000_000, LastUpdatedNs: now}
		},
		ReadLease: func(key LeaseDirtyKey) *model.Lease {
			return &model.Lease{PlatformID: key.PlatformID, Account: key.Account, NodeHash: "h1", ExpiryNs: now + int64(time.Hour), LastAccessedNs: now}
		},
		ReadSubscriptionNode: func(key SubscriptionNodeDirtyKey) *model.SubscriptionNode {
			return &model.SubscriptionNode{SubscriptionID: key.SubscriptionID, NodeHash: key.NodeHash, Tags: []string{"tag"}, Evicted: false}
		},
	}

	if err := engine.FlushDirtySets(readers); err != nil {
		t.Fatalf("flush dirty sets: %v", err)
	}

	statics, err := engine.LoadAllNodesStatic()
	if err != nil || len(statics) != 1 || statics[0].Hash != "h1" {
		t.Fatalf("nodes_static after flush: %v (n=%d)", err, len(statics))
	}
	dynamics, err := engine.LoadAllNodesDynamic()
	if err != nil || len(dynamics) != 1 || dynamics[0].FailureCount != 2 || dynamics[0].EgressIP != "9.9.9.9" {
		t.Fatalf("nodes_dynamic after flush: %v %+v", err, dynamics)
	}
	latencies, err := engine.LoadAllNodeLatency()
	if err != nil || len(latencies) != 1 || latencies[0].EwmaNs != 1_000_000 {
		t.Fatalf("node_latency after flush: %v %+v", err, latencies)
	}
	leases, err := engine.LoadAllLeases()
	if err != nil || len(leases) != 1 || leases[0].Account != "acct-1" {
		t.Fatalf("leases after flush: %v %+v", err, leases)
	}
	subNodes, err := engine.LoadAllSubscriptionNodes()
	if err != nil || len(subNodes) != 1 || subNodes[0].Evicted {
		t.Fatalf("subscription_nodes after flush: %v %+v", err, subNodes)
	}

	// A delete mark must reach the database too.
	engine.MarkNodeStaticDelete("h1")
	delReaders := readers
	if err := engine.FlushDirtySets(delReaders); err != nil {
		t.Fatalf("flush delete: %v", err)
	}
	statics, err = engine.LoadAllNodesStatic()
	if err != nil || len(statics) != 0 {
		t.Fatalf("nodes_static after delete: %v (n=%d)", err, len(statics))
	}
}

func TestPostgres_ConsistencyRepairRemovesOrphans(t *testing.T) {
	cfg := pgTestConfig(t)
	engine := newPGEngine(t, cfg)

	platform := model.Platform{
		ID: "p1", Name: "P1", StickyTTLNs: 1000, RegexFilters: []string{}, RegionFilters: []string{},
		AllocationPolicy: "BALANCED", ReverseProxyMissAction: "TREAT_AS_EMPTY",
		ReverseProxyEmptyAccountBehavior: "RANDOM", UpdatedAtNs: 1,
	}
	if err := engine.UpsertPlatform(platform); err != nil {
		t.Fatalf("seed platform: %v", err)
	}
	if err := engine.UpsertSubscription(model.Subscription{
		ID: "s1", Name: "S1", URL: "https://example.com", UpdateIntervalNs: int64(30 * time.Second),
		Enabled: true, EphemeralNodeEvictDelayNs: 1000, CreatedAtNs: 1, UpdatedAtNs: 1,
	}); err != nil {
		t.Fatalf("seed subscription: %v", err)
	}

	cacheDB := engine.CacheRepo.DB()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := cacheDB.Exec(query, args...); err != nil {
			t.Fatalf("seed cache row (%s): %v", query, err)
		}
	}
	exec(`INSERT INTO nodes_static (hash, raw_options_json, created_at_ns) VALUES (?, ?, ?)`, "keep", `{}`, 1)
	exec(`INSERT INTO subscription_nodes (subscription_id, node_hash, tags_json, evicted) VALUES (?, ?, ?, ?)`, "s1", "keep", "[]", false)
	exec(`INSERT INTO subscription_nodes (subscription_id, node_hash, tags_json, evicted) VALUES (?, ?, ?, ?)`, "missing-sub", "keep", "[]", false)
	exec(`INSERT INTO nodes_dynamic (hash) VALUES (?)`, "orphan-dynamic")
	exec(`INSERT INTO node_latency (node_hash, domain, ewma_ns, last_updated_ns) VALUES (?, ?, ?, ?)`, "orphan-latency", "example.com", 1, 1)
	exec(`INSERT INTO leases (platform_id, account, node_hash, expiry_ns, last_accessed_ns) VALUES (?, ?, ?, ?, ?)`, "missing-platform", "acct", "keep", 1, 1)
	exec(`INSERT INTO leases (platform_id, account, node_hash, expiry_ns, last_accessed_ns) VALUES (?, ?, ?, ?, ?)`, "p1", "acct", "keep", 1, 1)

	// Re-running bootstrap runs the same repair the server performs at startup.
	if err := repairConsistencyPostgres(cacheDB, cfg.StateSchema); err != nil {
		t.Fatalf("repair consistency: %v", err)
	}

	count := func(table string) int {
		t.Helper()
		var n int
		if err := cacheDB.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return n
	}
	if n := count("subscription_nodes"); n != 1 {
		t.Fatalf("subscription_nodes: want 1 row, got %d", n)
	}
	if n := count("nodes_dynamic"); n != 0 {
		t.Fatalf("nodes_dynamic: want 0 rows, got %d", n)
	}
	if n := count("node_latency"); n != 0 {
		t.Fatalf("node_latency: want 0 rows, got %d", n)
	}
	if n := count("leases"); n != 1 {
		t.Fatalf("leases: want 1 row, got %d", n)
	}
	if n := count("nodes_static"); n != 1 {
		t.Fatalf("nodes_static: want 1 row, got %d", n)
	}
}

func TestPostgres_CopyFromSQLite(t *testing.T) {
	cfg := pgTestConfig(t)
	target := newPGEngine(t, cfg)

	// Seed a legacy SQLite store through the normal engine.
	srcEngine, srcCloser, err := PersistenceBootstrap(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatalf("bootstrap sqlite source: %v", err)
	}
	defer srcCloser.Close()

	if err := srcEngine.UpsertPlatform(model.Platform{
		ID: "p1", Name: "CopiedPlatform", StickyTTLNs: 4242,
		RegexFilters: []string{"*x"}, RegionFilters: []string{},
		AllocationPolicy: "BALANCED", ReverseProxyMissAction: "TREAT_AS_EMPTY",
		ReverseProxyEmptyAccountBehavior: "RANDOM",
		PassiveCircuitBreakerDisabled:    true, UpdatedAtNs: 99,
	}); err != nil {
		t.Fatalf("seed sqlite platform: %v", err)
	}
	if err := srcEngine.UpsertSubscription(model.Subscription{
		ID: "s1", Name: "Copied Sub", URL: "https://example.com", UpdateIntervalNs: int64(time.Hour),
		Enabled: true, Ephemeral: false, IncrementalAliveNodes: true,
		EphemeralNodeEvictDelayNs: 10, CreatedAtNs: 1, UpdatedAtNs: 2,
	}); err != nil {
		t.Fatalf("seed sqlite subscription: %v", err)
	}
	ctx := context.Background()
	if err := srcEngine.CacheRepo.BulkUpsertNodesStatic([]model.NodeStatic{
		{Hash: "h1", RawOptions: []byte(`{"type":"socks"}`), CreatedAtNs: 1},
	}); err != nil {
		t.Fatalf("seed sqlite node: %v", err)
	}
	if err := srcEngine.CacheRepo.BulkUpsertSubscriptionNodes([]model.SubscriptionNode{
		{SubscriptionID: "s1", NodeHash: "h1", Tags: []string{"a"}, Evicted: true},
	}); err != nil {
		t.Fatalf("seed sqlite subscription node: %v", err)
	}

	// Dry run reports counts without writing.
	res, err := CopyTable(ctx, srcEngine.StateRepo.DB(), target.StateRepo.DB(), StateTables[1], 100, true)
	if err != nil {
		t.Fatalf("dry run copy: %v", err)
	}
	if res.Source != 1 || res.Written != 0 {
		t.Fatalf("dry run result: %+v", res)
	}

	for _, plan := range StateTables {
		if _, err := CopyTable(ctx, srcEngine.StateRepo.DB(), target.StateRepo.DB(), plan, 100, false); err != nil {
			t.Fatalf("copy state table %s: %v", plan.Name, err)
		}
	}
	for _, plan := range CacheTables {
		if _, err := CopyTable(ctx, srcEngine.CacheRepo.DB(), target.CacheRepo.DB(), plan, 100, false); err != nil {
			t.Fatalf("copy cache table %s: %v", plan.Name, err)
		}
	}

	if err := VerifyCopy(ctx, srcEngine.StateRepo.DB(), target.StateRepo.DB(), StateTables); err != nil {
		t.Fatalf("verify state copy: %v", err)
	}
	if err := VerifyCopy(ctx, srcEngine.CacheRepo.DB(), target.CacheRepo.DB(), CacheTables); err != nil {
		t.Fatalf("verify cache copy: %v", err)
	}

	gotPlatform, err := target.GetPlatform("p1")
	if err != nil {
		t.Fatalf("read copied platform: %v", err)
	}
	if gotPlatform.Name != "CopiedPlatform" || !gotPlatform.PassiveCircuitBreakerDisabled || gotPlatform.StickyTTLNs != 4242 {
		t.Fatalf("copied platform mismatch: %+v", gotPlatform)
	}
	subs, err := target.ListSubscriptions()
	if err != nil || len(subs) != 1 || !subs[0].Enabled || !subs[0].IncrementalAliveNodes {
		t.Fatalf("copied subscription mismatch: %v %+v", err, subs)
	}
	subNodes, err := target.LoadAllSubscriptionNodes()
	if err != nil || len(subNodes) != 1 || !subNodes[0].Evicted {
		t.Fatalf("copied subscription node mismatch (boolean conversion): %v %+v", err, subNodes)
	}

	// Re-running the copy is idempotent.
	for _, plan := range StateTables {
		if _, err := CopyTable(ctx, srcEngine.StateRepo.DB(), target.StateRepo.DB(), plan, 100, false); err != nil {
			t.Fatalf("re-copy state table %s: %v", plan.Name, err)
		}
	}
	plats, err := target.ListPlatforms()
	if err != nil || len(plats) != 1 {
		t.Fatalf("re-copy duplicated rows: %v (n=%d)", err, len(plats))
	}
}
