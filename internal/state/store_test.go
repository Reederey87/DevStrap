package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/zalando/go-keyring"
)

func TestMain(m *testing.M) {
	keyring.MockInit()
	os.Exit(m.Run())
}

func TestOpenConfiguresSQLiteAndSecuresFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var foreignKeys int
	if err := st.db.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 1 {
		t.Fatalf("foreign_keys = %d, want 1", foreignKeys)
	}

	var busyTimeout int
	if err := st.db.QueryRow("PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatal(err)
	}
	if busyTimeout <= 0 {
		t.Fatalf("busy_timeout = %d, want > 0", busyTimeout)
	}
	fkCheck, err := st.ForeignKeyCheck(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if fkCheck != "ok" {
		t.Fatalf("foreign key check = %q, want ok", fkCheck)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("state.db permissions = %s, want 0600", got)
	}
}

func TestOpenSnapshotIsReadOnlyAndCreatesNoWALSideFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")

	snap, err := OpenSnapshot(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	if _, err := snap.AllBlobRefs(t.Context()); err != nil {
		t.Fatalf("AllBlobRefs: %v", err)
	}
	if _, err := snap.db.ExecContext(t.Context(), `INSERT INTO local_meta(key, value) VALUES ('snapshot-write', 'no')`); err == nil {
		t.Fatal("write through OpenSnapshot unexpectedly succeeded")
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("snapshot created %s side file: %v", suffix, err)
		}
	}
}

func TestOpenRejectsExistingForeignKeyViolations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path}).String())
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`PRAGMA foreign_keys = OFF`,
		`CREATE TABLE parent (id TEXT PRIMARY KEY)`,
		`CREATE TABLE child (id TEXT PRIMARY KEY, parent_id TEXT NOT NULL REFERENCES parent(id))`,
		`INSERT INTO child (id, parent_id) VALUES ('child-1', 'missing-parent')`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			_ = db.Close()
			t.Fatalf("exec %q: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(context.Background(), path)
	if err == nil {
		_ = st.Close()
		t.Fatal("expected Open to reject foreign key violations")
	}
	if !strings.Contains(err.Error(), "foreign_key_check failed") {
		t.Fatalf("Open err = %v, want foreign_key_check failure", err)
	}
}

func TestTimestampFormatIsFixedWidthAndLexicallySortable(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	next := base.Add(time.Nanosecond)
	baseText := formatTimestamp(base)
	nextText := formatTimestamp(next)
	if len(baseText) != len(nextText) {
		t.Fatalf("timestamp lengths = %d/%d, want fixed width", len(baseText), len(nextText))
	}
	if baseText != "2026-06-25T12:00:00.000000000Z" {
		t.Fatalf("base timestamp = %q, want fixed-width UTC nanos", baseText)
	}
	if baseText >= nextText {
		t.Fatalf("timestamps not lexically sorted: %q >= %q", baseText, nextText)
	}
}

func TestSummaryBeforeMigrateIsFriendly(t *testing.T) {
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	for name, fn := range map[string]func() error{
		"Summary": func() error {
			_, err := st.Summary(ctx)
			return err
		},
		"CurrentDevice": func() error {
			_, err := st.CurrentDevice(ctx)
			return err
		},
		"ListProjects": func() error {
			_, err := st.ListProjects(ctx)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := fn(); !errors.Is(err, ErrNotInitialized) {
				t.Fatalf("%s error = %v, want ErrNotInitialized", name, err)
			}
		})
	}
}

func TestListWorktreesUsesDeterministicIDTieBreaker(t *testing.T) {
	ctx := context.Background()
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureWorkspace(ctx, "personal", "/tmp/Code"); err != nil {
		t.Fatal(err)
	}
	device, err := st.EnsureDevice(ctx, "test-device")
	if err != nil {
		t.Fatal(err)
	}
	project, err := st.UpsertProject(ctx, UpsertProjectParams{
		Path:          "work/acme/repo",
		Type:          "git_repo",
		RemoteURL:     "git@github.com:acme/repo.git",
		RemoteKey:     "github.com/acme/repo",
		DefaultBranch: "main",
		LocalPath:     filepath.Join(t.TempDir(), "repo"),
	})
	if err != nil {
		t.Fatal(err)
	}
	createdAt := formatTimestamp(time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC))
	for _, id := range []string{"wt_a", "wt_b"} {
		_, err := st.db.ExecContext(ctx, `
INSERT INTO worktrees (id, namespace_id, device_id, path, branch, base_ref, base_sha, created_by, status, dirty_state, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, 'origin/main', 'abc123', 'agent', 'active', 'clean', ?, ?);
`, id, project.ID, device.ID, filepath.Join(t.TempDir(), id), "agent/"+id, createdAt, createdAt)
		if err != nil {
			t.Fatal(err)
		}
	}
	worktrees, err := st.ListWorktrees(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(worktrees) != 2 {
		t.Fatalf("worktrees = %d, want 2", len(worktrees))
	}
	if worktrees[0].ID != "wt_b" || worktrees[1].ID != "wt_a" {
		t.Fatalf("worktree order = %s,%s; want wt_b,wt_a", worktrees[0].ID, worktrees[1].ID)
	}
}

func TestMigrateEnsureSummaryAndVersion(t *testing.T) {
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatalf("second migrate should be idempotent: %v", err)
	}
	version, err := st.Version()
	if err != nil {
		t.Fatal(err)
	}
	if version != 28 {
		t.Fatalf("schema version = %d, want 28", version)
	}

	var tableCount int
	if err := st.db.QueryRow(`
SELECT COUNT(*)
FROM sqlite_master
WHERE type = 'table' AND name IN (
  'workspaces', 'devices', 'namespace_entries', 'git_repos', 'draft_projects',
  'device_project_state', 'env_profiles', 'secret_bindings', 'worktrees',
  'agent_runs', 'events', 'jobs', 'conflicts', 'sync_cursors', 'event_delivery',
  'hub_cursors', 'draft_snapshots', 'workspace_keys', 'workspace_key_grants'
)`).Scan(&tableCount); err != nil {
		t.Fatal(err)
	}
	if tableCount != 19 {
		t.Fatalf("table count = %d, want 19", tableCount)
	}

	ctx := context.Background()
	if err := st.EnsureWorkspace(ctx, "personal", "/tmp/Code"); err != nil {
		t.Fatal(err)
	}
	workspaceID, err := st.WorkspaceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(workspaceID, "ws_") {
		t.Fatalf("workspace id = %q, want ws_ prefix", workspaceID)
	}
	if err := st.EnsureWorkspace(ctx, "personal-renamed", "/tmp/Code2"); err != nil {
		t.Fatal(err)
	}
	workspaceIDAgain, err := st.WorkspaceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if workspaceIDAgain != workspaceID {
		t.Fatalf("workspace id changed: %s -> %s", workspaceID, workspaceIDAgain)
	}
	if _, err := st.db.ExecContext(ctx, `
INSERT INTO workspaces (id, name, root_path, created_at, updated_at)
VALUES ('ws_second', 'second', '/tmp/Other', ?, ?);
`, timestampNow(), timestampNow()); err == nil {
		t.Fatal("expected singleton workspace constraint to reject a second workspace")
	}
	device, err := st.EnsureDevice(ctx, "test-device")
	if err != nil {
		t.Fatal(err)
	}
	again, err := st.EnsureDevice(ctx, "renamed-device")
	if err != nil {
		t.Fatal(err)
	}
	if device.ID != again.ID {
		t.Fatalf("device ID changed: %s -> %s", device.ID, again.ID)
	}
	summary, err := st.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.WorkspaceName != "personal-renamed" || summary.RootPath != "/tmp/Code2" || summary.ProjectCount != 0 {
		t.Fatalf("unexpected summary: %+v", summary)
	}
	if summary.DeviceID != device.ID {
		t.Fatalf("summary device = %q, want %q", summary.DeviceID, device.ID)
	}
}

func TestListProjectsUsesActiveNamespaceIndex(t *testing.T) {
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureWorkspace(context.Background(), "personal", "/tmp/Code"); err != nil {
		t.Fatal(err)
	}
	workspaceID, err := st.WorkspaceID(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rows, err := st.db.Query(`
EXPLAIN QUERY PLAN
SELECT n.id, n.path, n.path_key, n.type, COALESCE(n.display_name, ''), n.materialization_policy, n.status,
       COALESCE(n.source_event_hlc, 0), COALESCE(n.source_event_device_id, ''), COALESCE(n.source_event_id, ''),
       COALESCE(g.remote_url, ''), COALESCE(g.remote_key, ''), COALESCE(g.default_branch, ''), COALESCE(g.lfs_policy, ''),
       COALESCE(dps.local_path, ''), COALESCE(dps.materialization_state, ''), COALESCE(dps.dirty_state, '')
FROM namespace_entries n
LEFT JOIN git_repos g ON g.namespace_id = n.id
LEFT JOIN devices d ON d.trust_state = 'local'
LEFT JOIN device_project_state dps ON dps.namespace_id = n.id AND dps.device_id = d.id
WHERE n.workspace_id = ? AND n.status = 'active'
ORDER BY n.path_key;
`, workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	plan := strings.Join(details, "\n")
	if !strings.Contains(plan, "idx_namespace_active") {
		t.Fatalf("query plan = %s, want idx_namespace_active", plan)
	}
	if strings.Contains(plan, "USE TEMP B-TREE FOR ORDER BY") {
		t.Fatalf("query plan = %s, should not use temp b-tree for order by", plan)
	}
}

// TestBlobRefRewrapUsesCompositeIndexes pins that the exact-match (BINARY `= ?`)
// revoke/rewrap lookups and updates use the 00026 composite indexes rather than
// full-scanning. This is the P7-DATA-06 quadratic-scaling fix proper: the 00025
// NOCASE indexes only serve `col LIKE 'age_blob:%'` enumeration, and SQLite will
// not use a NOCASE index for a BINARY-collation equality comparison, so without
// the composite indexes each per-ref lookup in blob_gc.go's loop full-scans.
func TestBlobRefRewrapUsesCompositeIndexes(t *testing.T) {
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	explain := func(query string, args ...any) string {
		rows, err := st.db.Query("EXPLAIN QUERY PLAN "+query, args...)
		if err != nil {
			t.Fatalf("explain %q: %v", query, err)
		}
		defer rows.Close()
		var details []string
		for rows.Next() {
			var id, parent, notUsed int
			var detail string
			if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
				t.Fatal(err)
			}
			details = append(details, detail)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return strings.Join(details, "\n")
	}

	cases := []struct {
		name  string
		query string
		args  []any
		index string
	}{
		{
			name:  "DraftSnapshotsForBlobRef",
			query: "SELECT ds.namespace_id, n.path, ds.byte_size, ds.file_count FROM draft_snapshots ds JOIN namespace_entries n ON n.id = ds.namespace_id WHERE ds.blob_ref = ?;",
			args:  []any{"age_blob:deadbeef"},
			index: "idx_draft_snapshots_namespace_ref",
		},
		{
			name:  "UpdateBlobRef secret_bindings",
			query: "UPDATE secret_bindings SET encrypted_value_ref = ?, updated_at = ? WHERE encrypted_value_ref = ?;",
			args:  []any{"age_blob:new", "t", "age_blob:old"},
			index: "idx_secret_bindings_env_profile_ref",
		},
		{
			name:  "UpdateBlobRef draft_snapshots",
			query: "UPDATE draft_snapshots SET blob_ref = ? WHERE blob_ref = ?;",
			args:  []any{"age_blob:new", "age_blob:old"},
			index: "idx_draft_snapshots_namespace_ref",
		},
		{
			name:  "EnvProfilesForBlobRef",
			query: "SELECT DISTINCT n.id, n.path, e.name, e.provider, e.mode, e.id FROM namespace_entries n JOIN env_profiles e ON e.id = n.env_profile_id JOIN secret_bindings b ON b.env_profile_id = e.id WHERE n.status = 'active' AND b.encrypted_value_ref = ? ORDER BY n.path, e.name;",
			args:  []any{"age_blob:deadbeef"},
			index: "idx_secret_bindings_env_profile_ref",
		},
	}
	for _, tc := range cases {
		plan := explain(tc.query, tc.args...)
		if !strings.Contains(plan, tc.index) {
			t.Errorf("%s query plan = %q, want index %s", tc.name, plan, tc.index)
		}
		if strings.Contains(plan, "SCAN secret_bindings") && tc.index == "idx_secret_bindings_env_profile_ref" {
			t.Errorf("%s full-scans secret_bindings; plan = %q", tc.name, plan)
		}
		if strings.Contains(plan, "SCAN draft_snapshots") && tc.index == "idx_draft_snapshots_namespace_ref" {
			t.Errorf("%s full-scans draft_snapshots; plan = %q", tc.name, plan)
		}
	}
}

func TestMigrationDownAndUp(t *testing.T) {
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.Down(); err != nil {
		t.Fatal(err)
	}
	version, err := st.Version()
	if err != nil {
		t.Fatal(err)
	}
	if version != 26 {
		t.Fatalf("schema version after down = %d, want 26", version)
	}
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	version, err = st.Version()
	if err != nil {
		t.Fatal(err)
	}
	if version != 28 {
		t.Fatalf("schema version after re-migrate = %d, want 28", version)
	}
}

func TestMigration00023DownRefusesPopulatedCoordinates(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureWorkspace(ctx, "test", "/tmp/Code"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `
INSERT INTO env_profiles (
  id, workspace_id, name, provider, mode,
  source_event_hlc, source_event_device_id, source_event_id,
  created_at, updated_at
)
SELECT 'env_test', id, 'test', 'devstrap_encrypted', 'runtime_only',
       123, 'dev_source', 'evt_source', ?, ?
FROM workspaces;
`, timestampNow(), timestampNow()); err != nil {
		t.Fatal(err)
	}

	// Steps from 28 down to 23 are unrelated and must remain unaffected. Migration
	// 00027 doesn't exist in this branch (renumbered to 00028 to avoid a
	// collision), so goose's version sequence has no rung there: the first Down()
	// rolls 00028 straight back to 26, matching the applied-versions set actually
	// on disk.
	if err := st.Down(); err != nil { // 28 -> 26 (00027 doesn't exist)
		t.Fatal(err)
	}
	if err := st.Down(); err != nil { // 26 -> 25
		t.Fatal(err)
	}
	if err := st.Down(); err != nil { // 25 -> 24
		t.Fatal(err)
	}
	if err := st.Down(); err != nil { // 24 -> 23
		t.Fatal(err)
	}
	err = st.Down() // 23 -> 22 attempt, must refuse
	if err == nil {
		t.Fatal("migration 00023 down succeeded with populated source-event coordinates")
	}
	if !strings.Contains(err.Error(), "source-event coordinates") {
		t.Fatalf("migration 00023 down error = %q, want source-event coordinates refusal", err)
	}
	version, err := st.Version()
	if err != nil {
		t.Fatal(err)
	}
	if version != 23 {
		t.Fatalf("schema version after refused down = %d, want 23", version)
	}

	var hlc int64
	var deviceID, eventID string
	if err := st.db.QueryRowContext(ctx, `
SELECT source_event_hlc, source_event_device_id, source_event_id
FROM env_profiles
WHERE id = 'env_test';
`).Scan(&hlc, &deviceID, &eventID); err != nil {
		t.Fatalf("read preserved env coordinates: %v", err)
	}
	if hlc != 123 || deviceID != "dev_source" || eventID != "evt_source" {
		t.Fatalf("preserved coordinates = (%d, %q, %q), want (123, %q, %q)", hlc, deviceID, eventID, "dev_source", "evt_source")
	}
	for _, column := range []string{"source_event_hlc", "source_event_device_id", "source_event_id"} {
		if !envProfilesHasColumn(t, st, column) {
			t.Fatalf("env_profiles column %q missing after refused down", column)
		}
	}
}

func TestMigration00023DownEmptyCoordinatesSucceeds(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureWorkspace(ctx, "test", "/tmp/Code"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `
INSERT INTO env_profiles (id, workspace_id, name, provider, mode, created_at, updated_at)
SELECT 'env_test', id, 'test', 'devstrap_encrypted', 'runtime_only', ?, ?
FROM workspaces;
`, timestampNow(), timestampNow()); err != nil {
		t.Fatal(err)
	}

	if err := st.Down(); err != nil { // 28 -> 26 (00027 doesn't exist in this branch)
		t.Fatal(err)
	}
	if err := st.Down(); err != nil { // 26 -> 25
		t.Fatal(err)
	}
	if err := st.Down(); err != nil { // 25 -> 24
		t.Fatal(err)
	}
	if err := st.Down(); err != nil { // 24 -> 23
		t.Fatal(err)
	}
	if err := st.Down(); err != nil { // 23 -> 22
		t.Fatalf("migration 00023 down with empty coordinates: %v", err)
	}
	version, err := st.Version()
	if err != nil {
		t.Fatal(err)
	}
	if version != 22 {
		t.Fatalf("schema version after migration 00023 down = %d, want 22", version)
	}
	for _, column := range []string{"source_event_hlc", "source_event_device_id", "source_event_id"} {
		if envProfilesHasColumn(t, st, column) {
			t.Fatalf("env_profiles column %q remains after successful down", column)
		}
	}
	if err := st.Migrate(); err != nil {
		t.Fatalf("migrate after migration 00023 down: %v", err)
	}
	for _, column := range []string{"source_event_hlc", "source_event_device_id", "source_event_id"} {
		if !envProfilesHasColumn(t, st, column) {
			t.Fatalf("env_profiles column %q missing after re-migrate", column)
		}
	}
}

func envProfilesHasColumn(t *testing.T, st *Store, want string) bool {
	t.Helper()
	rows, err := st.db.Query(`PRAGMA table_info(env_profiles);`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		if name == want {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return false
}

func TestEventStateRollbackTogether(t *testing.T) {
	ctx := context.Background()
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureWorkspace(ctx, "test", "/tmp/Code"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureDevice(ctx, "device-a"); err != nil {
		t.Fatal(err)
	}
	err = st.WithTx(ctx, func(tx *Tx) error {
		event, err := st.InsertLocalEventTx(ctx, tx, Event{
			Type:        "project.added",
			PayloadJSON: `{"path":"work/acme/api"}`,
			ContentHash: ContentHash(`{"path":"work/acme/api"}`),
		})
		if err != nil {
			return err
		}
		_, err = tx.UpsertProject(ctx, UpsertProjectParams{
			Path:                  "../escape",
			Type:                  "git_repo",
			RemoteURL:             "git@github.com:acme/api.git",
			RemoteKey:             "github.com/acme/api",
			DefaultBranch:         "main",
			MaterializationPolicy: "lazy",
			SourceEventHLC:        event.HLC,
			SourceEventDeviceID:   event.DeviceID,
			SourceEventID:         event.ID,
		})
		return err
	})
	if err == nil {
		t.Fatal("expected invalid path to roll back the transaction")
	}
	events, err := st.PendingEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("events persisted after rollback: %+v", events)
	}
	projects, err := st.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 0 {
		t.Fatalf("projects persisted after rollback: %+v", projects)
	}
}

func TestEnsureDeviceConcurrentSecondCallerAdoptsWinner(t *testing.T) {
	ctx := context.Background()
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureWorkspace(ctx, "test", "/tmp/Code"); err != nil {
		t.Fatal(err)
	}
	first, err := st.EnsureDevice(ctx, "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.EnsureDevice(ctx, "second")
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Fatalf("second EnsureDevice ID = %s, want winner %s", second.ID, first.ID)
	}
	if second.Name != "second" {
		t.Fatalf("second EnsureDevice name = %q, want update to second", second.Name)
	}
}

func TestSingleLocalDeviceIndexRejectsSecondLocalRow(t *testing.T) {
	ctx := context.Background()
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureWorkspace(ctx, "test", "/tmp/Code"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureDevice(ctx, "device-a"); err != nil {
		t.Fatal(err)
	}
	_, err = st.db.ExecContext(ctx, `
INSERT INTO devices (id, name, os, arch, hostname, trust_state, last_seen_at, created_at, updated_at)
VALUES ('dev_second', 'second', 'linux', 'amd64', 'second', 'local', ?, ?, ?);
`, timestampNow(), timestampNow(), timestampNow())
	if err == nil {
		t.Fatal("expected single-local-device unique index to reject a second local row")
	}
	if !strings.Contains(err.Error(), "UNIQUE") && !strings.Contains(err.Error(), "constraint") {
		t.Fatalf("second local insert err = %v, want constraint failure", err)
	}
}

func TestUpsertProjectPreservesExistingLFSPolicyWhenUnspecified(t *testing.T) {
	ctx := context.Background()
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureWorkspace(ctx, "test", "/tmp/Code"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureDevice(ctx, "device-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertProject(ctx, UpsertProjectParams{
		Path:          "work/acme/lfs",
		Type:          "git_repo",
		RemoteURL:     "git@github.com:acme/lfs.git",
		RemoteKey:     "github.com/acme/lfs",
		DefaultBranch: "main",
		LFSPolicy:     "agent",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertProject(ctx, UpsertProjectParams{
		Path:          "work/acme/lfs",
		Type:          "git_repo",
		RemoteURL:     "git@github.com:acme/lfs.git",
		RemoteKey:     "github.com/acme/lfs",
		DefaultBranch: "main",
	}); err != nil {
		t.Fatal(err)
	}
	project, err := st.ProjectByPath(ctx, "work/acme/lfs")
	if err != nil {
		t.Fatal(err)
	}
	if project.LFSPolicy != "agent" {
		t.Fatalf("LFSPolicy = %q, want agent", project.LFSPolicy)
	}
}

func TestConcurrentWritesDoNotReturnBusy(t *testing.T) {
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- st.EnsureWorkspace(context.Background(), "ws", filepath.Join("/tmp", "Code", fmt.Sprintf("%02d", i)))
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestInsertLocalEventPersistsClockAndSequenceAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureWorkspace(ctx, "test", "/tmp/Code"); err != nil {
		t.Fatal(err)
	}
	device, err := st.EnsureDevice(ctx, "device-a")
	if err != nil {
		t.Fatal(err)
	}
	first, err := st.InsertLocalEvent(ctx, Event{Type: "project.added", PayloadJSON: `{"path":"work/a"}`})
	if err != nil {
		t.Fatal(err)
	}
	if first.DeviceID != device.ID || first.Seq != 1 || first.HLC == 0 {
		t.Fatalf("first local event = %+v, want device, seq=1, nonzero hlc", first)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st, err = Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	second, err := st.InsertLocalEvent(ctx, Event{Type: "project.updated", PayloadJSON: `{"path":"work/b"}`})
	if err != nil {
		t.Fatal(err)
	}
	if second.Seq != 2 {
		t.Fatalf("second seq = %d, want 2", second.Seq)
	}
	if second.HLC <= first.HLC {
		t.Fatalf("second HLC = %d, want > first %d", second.HLC, first.HLC)
	}
	if second.PrevEventHash != first.ContentHash {
		t.Fatalf("second prev hash = %q, want %q", second.PrevEventHash, first.ContentHash)
	}
}

func TestInsertLocalEventSignsAndRejectsTampering(t *testing.T) {
	ctx := context.Background()
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureWorkspace(ctx, "test", "/tmp/Code"); err != nil {
		t.Fatal(err)
	}
	device, err := st.EnsureDevice(ctx, "device-a")
	if err != nil {
		t.Fatal(err)
	}
	event, err := st.InsertLocalEvent(ctx, Event{Type: "project.added", PayloadJSON: `{"path":"work/a"}`})
	if err != nil {
		t.Fatal(err)
	}
	if event.DeviceSig == "" {
		t.Fatal("local event has empty device signature")
	}
	device, err = st.CurrentDevice(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if device.SigningPublicKey == "" {
		t.Fatal("device signing public key is empty")
	}
	pending, err := st.PendingEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].DeviceSig != event.DeviceSig {
		t.Fatalf("pending events = %+v, want signed event", pending)
	}
	tampered := event
	tampered.ID = "evt_tampered"
	tampered.PayloadJSON = `{"path":"work/tampered"}`
	tampered.ContentHash = ContentHash(tampered.PayloadJSON)
	err = st.InsertEvent(ctx, tampered)
	if err == nil {
		t.Fatal("expected tampered signed event to be rejected")
	}
	if !strings.Contains(err.Error(), "device signature invalid") {
		t.Fatalf("tampered insert error = %v, want signature invalid", err)
	}
}

func TestInsertLocalEventTxMatchesInsertLocalEvent(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name   string
		insert func(context.Context, *Store, Event) (Event, error)
	}{
		{
			name: "store wrapper",
			insert: func(ctx context.Context, st *Store, event Event) (Event, error) {
				return st.InsertLocalEvent(ctx, event)
			},
		},
		{
			name: "transaction helper",
			insert: func(ctx context.Context, st *Store, event Event) (Event, error) {
				var stamped Event
				err := st.WithTx(ctx, func(tx *Tx) error {
					var err error
					stamped, err = st.InsertLocalEventTx(ctx, tx, event)
					return err
				})
				return stamped, err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			if err := st.Migrate(); err != nil {
				t.Fatal(err)
			}
			if err := st.EnsureWorkspace(ctx, "test", "/tmp/Code"); err != nil {
				t.Fatal(err)
			}
			device, err := st.EnsureDevice(ctx, "device-a")
			if err != nil {
				t.Fatal(err)
			}

			first, err := tt.insert(ctx, st, Event{ID: "evt_p6_data_01", Type: "project.added", PayloadJSON: `{"path":"work/a"}`})
			if err != nil {
				t.Fatal(err)
			}
			if first.ID != "evt_p6_data_01" || first.DeviceID != device.ID || first.WorkspaceID == "" || first.Seq != 1 || first.HLC == 0 {
				t.Fatalf("first event = %+v, want stamped local event", first)
			}
			if first.CreatedAt == "" || first.ContentHash != ContentHash(first.PayloadJSON) || first.DeviceSig == "" {
				t.Fatalf("first event = %+v, want defaults, content hash, and signature", first)
			}

			second, err := tt.insert(ctx, st, Event{Type: "project.updated", PayloadJSON: `{"path":"work/b"}`})
			if err != nil {
				t.Fatal(err)
			}
			if second.Seq != 2 || second.HLC <= first.HLC {
				t.Fatalf("second event = %+v, want seq/HLC advance after %+v", second, first)
			}
			if second.PrevEventHash != first.ContentHash {
				t.Fatalf("second prev hash = %q, want %q", second.PrevEventHash, first.ContentHash)
			}

			_, err = tt.insert(ctx, st, Event{ID: first.ID, Type: "project.added", PayloadJSON: `{"path":"work/divergent"}`})
			if !errors.Is(err, ErrDivergentEvent) {
				t.Fatalf("duplicate err = %v, want ErrDivergentEvent", err)
			}
		})
	}
}

func TestInsertEventVerificationFailuresWrapSentinel(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name    string
		event   func(t *testing.T, st *Store, signed Event, device Device) Event
		wantErr string
	}{
		{
			name: "content hash mismatch",
			event: func(t *testing.T, st *Store, signed Event, device Device) Event {
				t.Helper()
				failing := signed
				failing.ID = "evt_content_hash_mismatch"
				failing.ContentHash = "sha256:wrong"
				return failing
			},
			wantErr: "content hash mismatch",
		},
		{
			name: "unknown device",
			event: func(t *testing.T, st *Store, signed Event, device Device) Event {
				t.Helper()
				failing := signed
				failing.ID = "evt_unknown_device"
				failing.DeviceID = "dev_unknown"
				failing.Type = "project.deleted"
				failing.DeviceSig = ""
				return failing
			},
			wantErr: "requires a signature from a known approved device",
		},
		{
			name: "no signing key",
			event: func(t *testing.T, st *Store, signed Event, device Device) Event {
				t.Helper()
				if err := st.UpsertDevice(ctx, Device{
					ID:         "dev_no_signing_key",
					Name:       "no-signing-key",
					OS:         "darwin",
					Arch:       "arm64",
					TrustState: "approved",
				}); err != nil {
					t.Fatal(err)
				}
				failing := signed
				failing.ID = "evt_no_signing_key"
				failing.DeviceID = "dev_no_signing_key"
				failing.Type = "project.deleted"
				failing.DeviceSig = ""
				return failing
			},
			wantErr: "requires a signature from a device with a signing key",
		},
		{
			name: "must-verify missing signature",
			event: func(t *testing.T, st *Store, signed Event, device Device) Event {
				t.Helper()
				if err := st.UpsertDevice(ctx, Device{
					ID:               "dev_missing_required_signature",
					Name:             "missing-required-signature",
					OS:               "darwin",
					Arch:             "arm64",
					SigningPublicKey: device.SigningPublicKey,
					TrustState:       "approved",
				}); err != nil {
					t.Fatal(err)
				}
				failing := signed
				failing.ID = "evt_missing_required_signature"
				failing.DeviceID = "dev_missing_required_signature"
				failing.Type = "project.deleted"
				failing.DeviceSig = ""
				return failing
			},
			wantErr: "requires a device signature",
		},
		{
			name: "non-must-verify missing signature",
			event: func(t *testing.T, st *Store, signed Event, device Device) Event {
				t.Helper()
				if err := st.UpsertDevice(ctx, Device{
					ID:               "dev_missing_optional_signature",
					Name:             "missing-optional-signature",
					OS:               "darwin",
					Arch:             "arm64",
					SigningPublicKey: device.SigningPublicKey,
					TrustState:       "approved",
				}); err != nil {
					t.Fatal(err)
				}
				failing := signed
				failing.ID = "evt_missing_optional_signature"
				failing.DeviceID = "dev_missing_optional_signature"
				failing.Type = "project.added"
				failing.DeviceSig = ""
				return failing
			},
			wantErr: "missing device signature",
		},
		{
			name: "not approved trust state",
			event: func(t *testing.T, st *Store, signed Event, device Device) Event {
				t.Helper()
				if err := st.UpsertDevice(ctx, Device{
					ID:               "dev_not_approved",
					Name:             "not-approved",
					OS:               "darwin",
					Arch:             "arm64",
					SigningPublicKey: device.SigningPublicKey,
					TrustState:       "pending",
				}); err != nil {
					t.Fatal(err)
				}
				failing := signed
				failing.ID = "evt_not_approved"
				failing.DeviceID = "dev_not_approved"
				failing.Type = "project.deleted"
				failing.DeviceSig = "not-empty"
				return failing
			},
			wantErr: "requires a signature from an approved device",
		},
		{
			name: "invalid signature",
			event: func(t *testing.T, st *Store, signed Event, device Device) Event {
				t.Helper()
				failing := signed
				failing.ID = "evt_invalid_signature"
				failing.PayloadJSON = `{"path":"work/tampered"}`
				failing.ContentHash = ContentHash(failing.PayloadJSON)
				return failing
			},
			wantErr: "device signature invalid",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st, signed, device := newSignedEventTestStore(t, ctx)
			err := st.InsertEvent(ctx, tt.event(t, st, signed, device))
			if err == nil {
				t.Fatal("expected verification error")
			}
			if !errors.Is(err, ErrEventVerification) {
				t.Fatalf("error = %v, want ErrEventVerification", err)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want message containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestVerifyEventSignatureDBErrorDoesNotWrapSentinel(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `CREATE TABLE devices (id TEXT PRIMARY KEY, trust_state TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	err = verifyEventSignature(ctx, db, Event{
		ID:          "evt_db_error",
		DeviceID:    "dev_db_error",
		Type:        "project.deleted",
		PayloadJSON: `{"path":"work/a"}`,
		ContentHash: ContentHash(`{"path":"work/a"}`),
	})
	if err == nil {
		t.Fatal("expected db error")
	}
	if errors.Is(err, ErrEventVerification) {
		t.Fatalf("error = %v, did not want ErrEventVerification", err)
	}
	if !strings.Contains(err.Error(), "read device signing public key") {
		t.Fatalf("error = %v, want read device signing public key", err)
	}
}

func TestVerifyRemoteEventMatchesInsertEventRegime(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name     string
		enrolled bool
		setup    func(t *testing.T, st *Store, local Device) Event
	}{
		{
			name: "local device",
			setup: func(t *testing.T, st *Store, local Device) Event {
				t.Helper()
				signing, err := devicekeys.NewSigningIdentity()
				if err != nil {
					t.Fatal(err)
				}
				if err := st.SetDeviceSigningPublicKey(ctx, local.ID, signing.Public); err != nil {
					t.Fatal(err)
				}
				return signedGrantEvent(t, local.ID, signing.Private, "evt_local")
			},
		},
		{
			name: "approved device valid signature",
			setup: func(t *testing.T, st *Store, local Device) Event {
				t.Helper()
				signing, err := devicekeys.NewSigningIdentity()
				if err != nil {
					t.Fatal(err)
				}
				if err := st.UpsertDevice(ctx, Device{
					ID: "dev_approved", Name: "approved", OS: "linux", Arch: "arm64",
					SigningPublicKey: signing.Public, TrustState: "approved",
				}); err != nil {
					t.Fatal(err)
				}
				return signedGrantEvent(t, "dev_approved", signing.Private, "evt_approved_valid")
			},
		},
		{
			name: "approved device forged signature",
			setup: func(t *testing.T, st *Store, local Device) Event {
				t.Helper()
				signing, err := devicekeys.NewSigningIdentity()
				if err != nil {
					t.Fatal(err)
				}
				if err := st.UpsertDevice(ctx, Device{
					ID: "dev_approved", Name: "approved", OS: "linux", Arch: "arm64",
					SigningPublicKey: signing.Public, TrustState: "approved",
				}); err != nil {
					t.Fatal(err)
				}
				event := signedGrantEvent(t, "dev_approved", signing.Private, "evt_approved_forged")
				event.PayloadJSON = `{"epoch":1,"recipient":"age1tampered","wrapped_key":"wrapped"}`
				event.ContentHash = ContentHash(event.PayloadJSON)
				return event
			},
		},
		{
			name: "revoked device",
			setup: func(t *testing.T, st *Store, local Device) Event {
				t.Helper()
				signing, err := devicekeys.NewSigningIdentity()
				if err != nil {
					t.Fatal(err)
				}
				if err := st.UpsertDevice(ctx, Device{
					ID: "dev_revoked", Name: "revoked", OS: "linux", Arch: "arm64",
					SigningPublicKey: signing.Public, TrustState: "revoked",
				}); err != nil {
					t.Fatal(err)
				}
				return signedGrantEvent(t, "dev_revoked", signing.Private, "evt_revoked")
			},
		},
		{
			name: "unknown device",
			setup: func(t *testing.T, st *Store, local Device) Event {
				t.Helper()
				return Event{
					ID: "evt_unknown", DeviceID: "dev_unknown", HLC: 1,
					Type:        "device.key.granted",
					PayloadJSON: `{"epoch":1,"recipient":"age1example","wrapped_key":"wrapped"}`,
					ContentHash: ContentHash(`{"epoch":1,"recipient":"age1example","wrapped_key":"wrapped"}`),
				}
			},
		},
	}

	for _, tt := range tests {
		for _, enrolled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/enrolled=%t", tt.name, enrolled), func(t *testing.T) {
				st, local := newVerifyRemoteEventTestStore(t, ctx)
				if enrolled {
					addApprovedDeviceForVerifierTest(t, ctx, st, "dev_enrolled")
				}
				event := tt.setup(t, st, local)

				got := st.VerifyRemoteEvent(ctx, event)
				want := verifyEventSignature(ctx, st.db, event)
				if (got == nil) != (want == nil) {
					t.Fatalf("VerifyRemoteEvent error = %v, verifyEventSignature error = %v", got, want)
				}
				if got != nil && errors.Is(got, ErrEventVerification) != errors.Is(want, ErrEventVerification) {
					t.Fatalf("VerifyRemoteEvent error = %v, verifyEventSignature error = %v", got, want)
				}
			})
		}
	}
}

// VerifyRemoteEvent must reject a content-hash mismatch (like insertEvent),
// even for an otherwise-valid approved-device signature, so the pre-ingest gate
// never lets the keyring advance from a carrier ApplyEvents would quarantine.
func TestVerifyRemoteEventRejectsContentHashMismatch(t *testing.T) {
	ctx := context.Background()
	st, _ := newVerifyRemoteEventTestStore(t, ctx)
	signing, err := devicekeys.NewSigningIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertDevice(ctx, Device{
		ID: "dev_approved", Name: "approved", OS: "linux", Arch: "arm64",
		SigningPublicKey: signing.Public, TrustState: "approved",
	}); err != nil {
		t.Fatal(err)
	}
	event := signedGrantEvent(t, "dev_approved", signing.Private, "evt_hash_mismatch")
	event.ContentHash = "sha256:deadbeef" // inconsistent with PayloadJSON
	err = st.VerifyRemoteEvent(ctx, event)
	if err == nil || !errors.Is(err, ErrEventVerification) {
		t.Fatalf("VerifyRemoteEvent err = %v, want ErrEventVerification for content-hash mismatch", err)
	}
}

func newVerifyRemoteEventTestStore(t *testing.T, ctx context.Context) (*Store, Device) {
	t.Helper()
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureWorkspace(ctx, "test", "/tmp/Code"); err != nil {
		t.Fatal(err)
	}
	local, err := st.EnsureDevice(ctx, "device-a")
	if err != nil {
		t.Fatal(err)
	}
	return st, local
}

func addApprovedDeviceForVerifierTest(t *testing.T, ctx context.Context, st *Store, deviceID string) {
	t.Helper()
	signing, err := devicekeys.NewSigningIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertDevice(ctx, Device{
		ID: deviceID, Name: deviceID, OS: "linux", Arch: "arm64",
		SigningPublicKey: signing.Public, TrustState: "approved",
	}); err != nil {
		t.Fatal(err)
	}
}

// P6-SYNC-03: enrollment is sticky. Revoked/lost rows prove an enrollment
// happened, so the fail-closed verification regime must survive revoking the
// last approved device; pending placeholders alone must not close the window.
func TestHasEnrolledDevicesStickyAfterRevoke(t *testing.T) {
	ctx := context.Background()
	st, _ := newVerifyRemoteEventTestStore(t, ctx)

	assertEnrolled := func(want bool, step string) {
		t.Helper()
		got, err := hasEnrolledDevices(ctx, st.db)
		if err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		if got != want {
			t.Fatalf("%s: hasEnrolledDevices = %t, want %t", step, got, want)
		}
	}

	// Only the local device: the bootstrap window is open.
	assertEnrolled(false, "local only")

	// An auto-created pending placeholder (EnsureRemoteDeviceTx) must not count.
	if err := st.UpsertDevice(ctx, Device{
		ID: "dev_pending", Name: "pending", OS: "linux", Arch: "arm64", TrustState: "pending",
	}); err != nil {
		t.Fatal(err)
	}
	assertEnrolled(false, "pending placeholder")

	// Approving a device closes the window.
	if err := st.UpsertDevice(ctx, Device{
		ID: "dev_b", Name: "b", OS: "linux", Arch: "arm64", TrustState: "approved",
	}); err != nil {
		t.Fatal(err)
	}
	assertEnrolled(true, "approved")

	// Revoking the LAST approved device keeps it closed (sticky).
	if err := st.SetDeviceTrustState(ctx, "dev_b", "revoked"); err != nil {
		t.Fatal(err)
	}
	assertEnrolled(true, "revoked last approved")

	// Lost counts the same way.
	if err := st.SetDeviceTrustState(ctx, "dev_b", "lost"); err != nil {
		t.Fatal(err)
	}
	assertEnrolled(true, "lost last approved")
}

// TestEventSignatureV2BindsDeviceIDAndSeq pins the P6-SYNC-04 signature-domain
// upgrade: new local events are signed under devstrap:event:v2 (payload
// includes DeviceID and Seq), verification accepts v2 first and falls back to
// v1 for legacy events, and a v2-signed event with a tampered DeviceID or Seq
// fails BOTH domains (v2 because the payload changed, v1 because the domain
// differs) — so the re-attribution the enc.v2 AAD blocks on the encrypted
// plane is also signature-blocked on the plaintext plane.
func TestEventSignatureV2BindsDeviceIDAndSeq(t *testing.T) {
	ctx := context.Background()
	st, _ := newVerifyRemoteEventTestStore(t, ctx)
	signing, err := devicekeys.NewSigningIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertDevice(ctx, Device{
		ID: "dev_v2", Name: "v2", OS: "linux", Arch: "arm64",
		SigningPublicKey: signing.Public, TrustState: "approved",
	}); err != nil {
		t.Fatal(err)
	}
	base := Event{
		ID: "evt_v2", DeviceID: "dev_v2", Seq: 3, HLC: 9,
		Type:        "device.key.granted",
		PayloadJSON: `{"epoch":1,"recipient":"age1example","wrapped_key":"wrapped"}`,
		ContentHash: ContentHash(`{"epoch":1,"recipient":"age1example","wrapped_key":"wrapped"}`),
	}

	// v2-signed verifies.
	v2 := base
	sig, err := devicekeys.Sign(signing.Private, eventSignatureDomainV2, EventSignaturePayloadV2(v2))
	if err != nil {
		t.Fatal(err)
	}
	v2.DeviceSig = sig
	if err := st.VerifyRemoteEvent(ctx, v2); err != nil {
		t.Fatalf("v2-signed event failed verification: %v", err)
	}

	// Tampered DeviceID on a v2 signature is rejected. (The devices row for
	// the re-attributed ID must exist with the same public key to isolate the
	// signature check from the unknown-device branch.)
	if err := st.UpsertDevice(ctx, Device{
		ID: "dev_other", Name: "other", OS: "linux", Arch: "arm64",
		SigningPublicKey: signing.Public, TrustState: "approved",
	}); err != nil {
		t.Fatal(err)
	}
	reattributed := v2
	reattributed.DeviceID = "dev_other"
	if err := st.VerifyRemoteEvent(ctx, reattributed); !errors.Is(err, ErrEventVerification) {
		t.Fatalf("re-attributed v2 event: got %v, want ErrEventVerification", err)
	}

	// Tampered Seq on a v2 signature is rejected.
	reseq := v2
	reseq.Seq = 1
	if err := st.VerifyRemoteEvent(ctx, reseq); !errors.Is(err, ErrEventVerification) {
		t.Fatalf("re-sequenced v2 event: got %v, want ErrEventVerification", err)
	}

	// Legacy v1-signed event still verifies via the fallback (re-founded hubs
	// re-push v1-signed history verbatim).
	v1 := base
	v1.ID = "evt_v1_legacy"
	v1sig, err := devicekeys.Sign(signing.Private, eventSignatureDomain, EventSignaturePayload(v1))
	if err != nil {
		t.Fatal(err)
	}
	v1.DeviceSig = v1sig
	if err := st.VerifyRemoteEvent(ctx, v1); err != nil {
		t.Fatalf("legacy v1-signed event failed verification: %v", err)
	}

	// A garbage signature still fails both domains.
	bad := base
	bad.ID = "evt_bad"
	bad.DeviceSig = v2.DeviceSig[:len(v2.DeviceSig)-4] + "AAAA"
	if err := st.VerifyRemoteEvent(ctx, bad); !errors.Is(err, ErrEventVerification) {
		t.Fatalf("garbage signature: got %v, want ErrEventVerification", err)
	}
}

func signedGrantEvent(t *testing.T, deviceID, privateSigningKey, eventID string) Event {
	t.Helper()
	event := Event{
		ID: eventID, DeviceID: deviceID, HLC: 1,
		Type:        "device.key.granted",
		PayloadJSON: `{"epoch":1,"recipient":"age1example","wrapped_key":"wrapped"}`,
		ContentHash: ContentHash(`{"epoch":1,"recipient":"age1example","wrapped_key":"wrapped"}`),
	}
	sig, err := devicekeys.Sign(privateSigningKey, eventSignatureDomain, EventSignaturePayload(event))
	if err != nil {
		t.Fatal(err)
	}
	event.DeviceSig = sig
	return event
}

func newSignedEventTestStore(t *testing.T, ctx context.Context) (*Store, Event, Device) {
	t.Helper()
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureWorkspace(ctx, "test", "/tmp/Code"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureDevice(ctx, "device-a"); err != nil {
		t.Fatal(err)
	}
	event, err := st.InsertLocalEvent(ctx, Event{Type: "project.added", PayloadJSON: `{"path":"work/a"}`})
	if err != nil {
		t.Fatal(err)
	}
	device, err := st.CurrentDevice(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if event.DeviceSig == "" {
		t.Fatal("local event has empty device signature")
	}
	if device.SigningPublicKey == "" {
		t.Fatal("device signing public key is empty")
	}
	return st, event, device
}

func TestOpenConflictsByTypeFiltersOpenRows(t *testing.T) {
	ctx := context.Background()
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureWorkspace(ctx, "test", "/tmp/Code"); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertConflict(ctx, "", "wanted", `{"n":1}`); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertConflict(ctx, "", "other", `{"n":2}`); err != nil {
		t.Fatal(err)
	}
	wanted, err := st.OpenConflictsByType(ctx, "wanted")
	if err != nil {
		t.Fatal(err)
	}
	if len(wanted) != 1 || wanted[0].Type != "wanted" {
		t.Fatalf("wanted conflicts = %+v, want one wanted row", wanted)
	}
	if err := st.ResolveConflict(ctx, wanted[0].ID, `{"action":"test"}`); err != nil {
		t.Fatal(err)
	}
	wanted, err = st.OpenConflictsByType(ctx, "wanted")
	if err != nil {
		t.Fatal(err)
	}
	if len(wanted) != 0 {
		t.Fatalf("wanted conflicts after resolve = %+v, want none", wanted)
	}
}

func TestInsertLocalEventSeedsClockFromExistingEvents(t *testing.T) {
	ctx := context.Background()
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureWorkspace(ctx, "test", "/tmp/Code"); err != nil {
		t.Fatal(err)
	}
	device, err := st.EnsureDevice(ctx, "device-a")
	if err != nil {
		t.Fatal(err)
	}
	highHLC := packHLCForTest(time.Now().UTC().Add(time.Hour).UnixMilli(), 7)
	if err := st.InsertEvent(ctx, Event{
		ID:          "evt_seed",
		DeviceID:    device.ID,
		Seq:         9,
		HLC:         highHLC,
		Type:        "project.added",
		PayloadJSON: `{"path":"work/seed"}`,
	}); err != nil {
		t.Fatal(err)
	}
	next, err := st.InsertLocalEvent(ctx, Event{Type: "project.updated", PayloadJSON: `{"path":"work/next"}`})
	if err != nil {
		t.Fatal(err)
	}
	if next.Seq != 10 {
		t.Fatalf("seeded seq = %d, want 10", next.Seq)
	}
	if next.HLC <= highHLC {
		t.Fatalf("seeded HLC = %d, want > existing %d", next.HLC, highHLC)
	}
}

func packHLCForTest(physical, logical int64) int64 {
	return (physical << hlcLogicalBits) | logical
}

func TestUpsertEnvProfileTxLWWCoordsStamped(t *testing.T) {
	ctx := context.Background()
	st, project := newEnvProfileTestStore(t, ctx, "work/acme/api")
	defer st.Close()

	low := Event{ID: "evt_low", DeviceID: "dev_a", HLC: 10}
	if err := st.WithTx(ctx, func(tx *Tx) error {
		_, err := tx.UpsertEnvProfileTx(ctx, project.ID, EnvProfileParams{
			Name:     "default",
			Provider: "devstrap_encrypted",
			Mode:     "hydrate_or_runtime",
			BlobRef:  "age_blob:aaaa",
			VarNames: []string{"API_TOKEN"},
		}, low)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.WithTx(ctx, func(tx *Tx) error {
		hlc, deviceID, eventID, ok, err := tx.EnvProfileSourceCoords(ctx, project.ID)
		if err != nil {
			return err
		}
		if !ok || hlc != low.HLC || deviceID != low.DeviceID || eventID != low.ID {
			t.Fatalf("coords = (%d, %q, %q, %t), want low event", hlc, deviceID, eventID, ok)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	high := Event{ID: "evt_high", DeviceID: "dev_b", HLC: 11}
	if err := st.WithTx(ctx, func(tx *Tx) error {
		_, err := tx.UpsertEnvProfileTx(ctx, project.ID, EnvProfileParams{
			Name:     "default",
			Provider: "1password",
			Mode:     "runtime_only",
			Refs:     map[string]string{"API_TOKEN": "op://vault/item/token", "DB_URL": "op://vault/item/db"},
		}, high)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	profile, bindings, err := st.EnvProfileForProject(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Provider != "1password" || profile.Mode != "runtime_only" {
		t.Fatalf("profile = %#v, want provider runtime profile", profile)
	}
	if len(bindings) != 2 || bindings[0].EncryptedValueRef != "" || bindings[0].ProviderRef == "" {
		t.Fatalf("provider bindings = %#v", bindings)
	}
	if err := st.WithTx(ctx, func(tx *Tx) error {
		hlc, deviceID, eventID, ok, err := tx.EnvProfileSourceCoords(ctx, project.ID)
		if err != nil {
			return err
		}
		if !ok || hlc != high.HLC || deviceID != high.DeviceID || eventID != high.ID {
			t.Fatalf("coords = (%d, %q, %q, %t), want high event", hlc, deviceID, eventID, ok)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestEnvProfileLegacyWrappersUseNullSourceCoords(t *testing.T) {
	ctx := context.Background()
	st, project := newEnvProfileTestStore(t, ctx, "work/acme/api")
	defer st.Close()

	if _, err := st.SaveCapturedEnvProfile(ctx, project.ID, "", []string{"API_TOKEN"}, "age_blob:deadbeef"); err != nil {
		t.Fatal(err)
	}
	if err := st.WithTx(ctx, func(tx *Tx) error {
		_, _, _, ok, err := tx.EnvProfileSourceCoords(ctx, project.ID)
		if err != nil {
			return err
		}
		if ok {
			t.Fatal("EnvProfileSourceCoords ok = true, want false for legacy wrapper")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveProviderEnvProfile(ctx, project.ID, "", "1password", map[string]string{"API_TOKEN": "op://vault/item/token"}); err != nil {
		t.Fatal(err)
	}
	if err := st.WithTx(ctx, func(tx *Tx) error {
		_, _, _, ok, err := tx.EnvProfileSourceCoords(ctx, project.ID)
		if err != nil {
			return err
		}
		if ok {
			t.Fatal("EnvProfileSourceCoords ok = true after provider wrapper, want false")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestEnvProfilesForBlobRef(t *testing.T) {
	ctx := context.Background()
	st, projectA := newEnvProfileTestStore(t, ctx, "work/acme/api")
	defer st.Close()
	projectB, err := st.UpsertProject(ctx, UpsertProjectParams{
		Path:          "work/acme/web",
		Type:          "git_repo",
		RemoteURL:     "git@github.com:acme/web.git",
		RemoteKey:     "github.com/acme/web",
		DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveCapturedEnvProfile(ctx, projectA.ID, "default", []string{"API_TOKEN", "DB_URL"}, "age_blob:shared"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveCapturedEnvProfile(ctx, projectB.ID, "default", []string{"WEB_TOKEN"}, "age_blob:shared"); err != nil {
		t.Fatal(err)
	}

	refs, err := st.EnvProfilesForBlobRef(ctx, "age_blob:shared")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("refs len = %d, want 2: %#v", len(refs), refs)
	}
	if refs[0].Path != "work/acme/api" || strings.Join(refs[0].VarNames, ",") != "API_TOKEN,DB_URL" {
		t.Fatalf("first ref = %#v", refs[0])
	}
	if refs[1].Path != "work/acme/web" || strings.Join(refs[1].VarNames, ",") != "WEB_TOKEN" {
		t.Fatalf("second ref = %#v", refs[1])
	}
	absent, err := st.EnvProfilesForBlobRef(ctx, "age_blob:absent")
	if err != nil {
		t.Fatal(err)
	}
	if len(absent) != 0 {
		t.Fatalf("absent refs = %#v, want none", absent)
	}
}

// TestOpenSnapshotFreezesBlobRefs proves snapshot enumeration freezes the row-set: a
// binding added to the live store after VACUUM INTO is invisible to the backup
// file (P7-DATA-03).
func TestOpenSnapshotFreezesBlobRefs(t *testing.T) {
	ctx := context.Background()
	st, project := newEnvProfileTestStore(t, ctx, "work/acme/api")
	defer st.Close()

	const first = "age_blob:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := st.SaveCapturedEnvProfile(ctx, project.ID, "default", []string{"API_TOKEN"}, first); err != nil {
		t.Fatal(err)
	}

	backupPath := filepath.Join(t.TempDir(), "snap.db")
	if err := st.Backup(ctx, backupPath); err != nil {
		t.Fatal(err)
	}

	// Live store gains a second binding after the snapshot was taken.
	projectB, err := st.UpsertProject(ctx, UpsertProjectParams{
		Path:          "work/acme/web",
		Type:          "git_repo",
		RemoteURL:     "git@github.com:acme/web.git",
		RemoteKey:     "github.com/acme/web",
		DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	const second = "age_blob:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err := st.SaveCapturedEnvProfile(ctx, projectB.ID, "default", []string{"WEB_TOKEN"}, second); err != nil {
		t.Fatal(err)
	}

	live, err := st.AllBlobRefs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 2 {
		t.Fatalf("live AllBlobRefs = %v, want both bindings", live)
	}

	snap, err := OpenSnapshot(ctx, backupPath)
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	frozen, err := snap.AllBlobRefs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(frozen) != 1 || frozen[0] != first {
		t.Fatalf("snapshot AllBlobRefs = %v, want only %q", frozen, first)
	}
}

func TestMustVerifyEventIncludesTrustAffectingTypes(t *testing.T) {
	for _, eventType := range []string{"project.deleted", "project.renamed", "env.profile.updated", "device.revoked", "device.lost"} {
		if !mustVerifyEvent(eventType) {
			t.Fatalf("mustVerifyEvent(%q) = false, want true", eventType)
		}
	}
	if mustVerifyEvent("draft.snapshot.created") {
		t.Fatal("mustVerifyEvent(draft.snapshot.created) = true, want false")
	}
}

func newEnvProfileTestStore(t *testing.T, ctx context.Context, path string) (*Store, NamespaceEntry) {
	t.Helper()
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureWorkspace(ctx, "personal", "/tmp/Code"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureDevice(ctx, "test-device"); err != nil {
		t.Fatal(err)
	}
	project, err := st.UpsertProject(ctx, UpsertProjectParams{
		Path:          path,
		Type:          "git_repo",
		RemoteURL:     "git@github.com:acme/api.git",
		RemoteKey:     "github.com/acme/api",
		DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	return st, project
}

func TestMarkEncryptedBindingsNeedingRotation(t *testing.T) {
	ctx := context.Background()
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureWorkspace(ctx, "personal", "/tmp/Code"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureDevice(ctx, "test-device"); err != nil {
		t.Fatal(err)
	}
	project, err := st.UpsertProject(ctx, UpsertProjectParams{
		Path:          "work/acme/api",
		Type:          "git_repo",
		RemoteURL:     "git@github.com:acme/api.git",
		RemoteKey:     "github.com/acme/api",
		DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveCapturedEnvProfile(ctx, project.ID, "default", []string{"API_TOKEN", "DB_URL"}, "age_blob:deadbeef"); err != nil {
		t.Fatal(err)
	}

	if n, err := st.CountSecretBindingsNeedingRotation(ctx); err != nil || n != 0 {
		t.Fatalf("initial rotation count = %d, err = %v, want 0", n, err)
	}
	flagged, err := st.MarkEncryptedBindingsNeedingRotation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if flagged != 2 {
		t.Fatalf("flagged = %d, want 2", flagged)
	}
	if n, err := st.CountSecretBindingsNeedingRotation(ctx); err != nil || n != 2 {
		t.Fatalf("rotation count after flag = %d, err = %v, want 2", n, err)
	}
	_, bindings, err := st.EnvProfileForProject(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range bindings {
		if !b.NeedsRotation {
			t.Fatalf("binding %s not flagged needs_rotation", b.VarName)
		}
	}
	// Idempotent: a second call flags nothing new.
	if again, err := st.MarkEncryptedBindingsNeedingRotation(ctx); err != nil || again != 0 {
		t.Fatalf("second flag = %d, err = %v, want 0", again, err)
	}
}

func TestClearRotationForProject(t *testing.T) {
	ctx := context.Background()
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureWorkspace(ctx, "personal", "/tmp/Code"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureDevice(ctx, "test-device"); err != nil {
		t.Fatal(err)
	}
	projectA, err := st.UpsertProject(ctx, UpsertProjectParams{
		Path:          "work/acme/api",
		Type:          "git_repo",
		RemoteURL:     "git@github.com:acme/api.git",
		RemoteKey:     "github.com/acme/api",
		DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	projectB, err := st.UpsertProject(ctx, UpsertProjectParams{
		Path:          "work/acme/web",
		Type:          "git_repo",
		RemoteURL:     "git@github.com:acme/web.git",
		RemoteKey:     "github.com/acme/web",
		DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	varsA := []string{"API_TOKEN", "DB_URL"}
	varsB := []string{"API_TOKEN"}
	if _, err := st.SaveCapturedEnvProfile(ctx, projectA.ID, "default", varsA, "age_blob:deadbeef"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveCapturedEnvProfile(ctx, projectB.ID, "default", varsB, "age_blob:cafebabe"); err != nil {
		t.Fatal(err)
	}
	flagged, err := st.MarkEncryptedBindingsNeedingRotation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if flagged != len(varsA)+len(varsB) {
		t.Fatalf("flagged = %d, want %d", flagged, len(varsA)+len(varsB))
	}

	cleared, err := st.ClearRotationForProject(ctx, projectA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cleared != len(varsA) {
		t.Fatalf("cleared = %d, want %d", cleared, len(varsA))
	}
	_, bindingsA, err := st.EnvProfileForProject(ctx, projectA.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, binding := range bindingsA {
		if binding.NeedsRotation {
			t.Fatalf("project A binding %s still flagged needs_rotation", binding.VarName)
		}
	}
	_, bindingsB, err := st.EnvProfileForProject(ctx, projectB.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, binding := range bindingsB {
		if !binding.NeedsRotation {
			t.Fatalf("project B binding %s unexpectedly cleared needs_rotation", binding.VarName)
		}
	}
}

// TestApplyRemoteDeviceTrustTxMatrix pins the sticky/monotonic transition
// rules for synced device.revoked/device.lost (TRUST-01).
func TestApplyRemoteDeviceTrustTxMatrix(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureWorkspace(ctx, "test", "/tmp/Code"); err != nil {
		t.Fatal(err)
	}
	local, err := st.EnsureDevice(ctx, "local-device")
	if err != nil {
		t.Fatal(err)
	}
	seed := func(id, trust string) {
		t.Helper()
		if err := st.UpsertDevice(ctx, Device{ID: id, Name: id, OS: "linux", Arch: "arm64", TrustState: trust}); err != nil {
			t.Fatal(err)
		}
	}
	seed("dev-pending", "pending")
	seed("dev-approved", "approved")
	seed("dev-revoked", "revoked")
	seed("dev-lost", "lost")

	cases := []struct {
		target, to  string
		wantChanged bool
		wantState   string
	}{
		{"dev-pending", "revoked", true, "revoked"},    // pending -> revoked is fail-closed
		{"dev-approved", "lost", true, "lost"},         // approved -> lost flips
		{"dev-revoked", "lost", false, "revoked"},      // revoked <-> lost churn no-ops
		{"dev-lost", "revoked", false, "lost"},         // sticky both directions
		{local.ID, "revoked", false, local.TrustState}, // local device never flips remotely
	}
	for _, tc := range cases {
		var changed bool
		if err := st.WithTx(ctx, func(tx *Tx) error {
			var err error
			changed, err = tx.ApplyRemoteDeviceTrustTx(ctx, tc.target, tc.to, 0)
			return err
		}); err != nil {
			t.Fatalf("%s -> %s: %v", tc.target, tc.to, err)
		}
		if changed != tc.wantChanged {
			t.Fatalf("%s -> %s: changed=%v, want %v", tc.target, tc.to, changed, tc.wantChanged)
		}
		devices, err := st.ListDevices(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range devices {
			if d.ID == tc.target && d.TrustState != tc.wantState {
				t.Fatalf("%s -> %s: state=%q, want %q", tc.target, tc.to, d.TrustState, tc.wantState)
			}
		}
	}
	// approved is not a valid remote transition target.
	if err := st.WithTx(ctx, func(tx *Tx) error {
		_, err := tx.ApplyRemoteDeviceTrustTx(ctx, "dev-pending", "approved", 0)
		return err
	}); err == nil {
		t.Fatal("remote approve must be rejected — approval is the local ceremony only")
	}
}

// TestUpsertEnvProfileTxPreservesNeedsRotation (TRUST-01 dogfood finding): a
// superseding upsert (revoke rewrap repointing the blob ref) must carry each
// binding's needs_rotation flag forward — wiping it broke the P5-PROD-03
// doctor surfacing on the revoker and on every receiving device.
func TestUpsertEnvProfileTxPreservesNeedsRotation(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureWorkspace(ctx, "test", "/tmp/Code"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureDevice(ctx, "local-device"); err != nil {
		t.Fatal(err)
	}
	ns, err := st.UpsertProject(ctx, UpsertProjectParams{Path: "work/x", Type: "git_repo", RemoteURL: "https://example.com/x", RemoteKey: "example.com/x"})
	if err != nil {
		t.Fatal(err)
	}
	params := EnvProfileParams{Name: "default", Provider: "devstrap_encrypted", Mode: "hydrate_or_runtime", BlobRef: "age_blob:old", VarNames: []string{"API_TOKEN", "DB_URL"}}
	if err := st.WithTx(ctx, func(tx *Tx) error {
		_, err := tx.UpsertEnvProfileTx(ctx, ns.ID, params, Event{ID: "e1", DeviceID: "d1", HLC: 10})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MarkEncryptedBindingsNeedingRotation(ctx); err != nil {
		t.Fatal(err)
	}
	// Superseding upsert (rewrap: same vars, new ref) must keep the flags.
	params.BlobRef = "age_blob:new"
	if err := st.WithTx(ctx, func(tx *Tx) error {
		_, err := tx.UpsertEnvProfileTx(ctx, ns.ID, params, Event{ID: "e2", DeviceID: "d1", HLC: 20})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	n, err := st.CountSecretBindingsNeedingRotation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("needs_rotation after superseding upsert = %d, want 2 (flags preserved)", n)
	}
	// And the operator clear still works on the fresh rows.
	if _, err := st.ClearRotationForProject(ctx, ns.ID); err != nil {
		t.Fatal(err)
	}
	if n, err = st.CountSecretBindingsNeedingRotation(ctx); err != nil || n != 0 {
		t.Fatalf("after clear: n=%d err=%v", n, err)
	}
}

// TestLatestDraftSnapshotDeterministicTiebreak (P7-SYNC-03): on an HLC tie,
// LatestDraftSnapshot must pick the lexicographically-highest
// (source_event_device_id, source_event_id), not local created_at/id. Winner
// is inserted first so created_at DESC / id DESC would prefer the loser.
func TestLatestDraftSnapshotDeterministicTiebreak(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureWorkspace(ctx, "personal", "/tmp/Code"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureDevice(ctx, "test-device"); err != nil {
		t.Fatal(err)
	}
	project, err := st.UpsertProject(ctx, UpsertProjectParams{
		Path: "work/draft-tie",
		Type: "draft_project",
	})
	if err != nil {
		t.Fatal(err)
	}

	const sameHLC int64 = 1_700_000_000_000
	// Canonical winner: higher device id (and higher event id) — insert first.
	winner := Event{ID: "evt_z_winner", DeviceID: "device_z", HLC: sameHLC}
	// Canonical loser: lower device id — insert second so local wall-clock/
	// snap id would win under the old ORDER BY created_at DESC, id DESC.
	loser := Event{ID: "evt_a_loser", DeviceID: "device_a", HLC: sameHLC}

	if err := st.RecordDraftSnapshot(ctx, project.ID, "age_blob:"+strings.Repeat("a", 64), 10, 1, winner); err != nil {
		t.Fatalf("RecordDraftSnapshot winner: %v", err)
	}
	if err := st.RecordDraftSnapshot(ctx, project.ID, "age_blob:"+strings.Repeat("b", 64), 20, 2, loser); err != nil {
		t.Fatalf("RecordDraftSnapshot loser: %v", err)
	}

	// Force created_at so old ordering demonstrably prefers the loser even if
	// both RecordDraftSnapshot calls shared the same nanosecond timestamp.
	early := formatTimestamp(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	late := formatTimestamp(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	if _, err := st.db.ExecContext(ctx, `
UPDATE draft_snapshots SET created_at = ? WHERE source_event_id = ?;
`, early, winner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `
UPDATE draft_snapshots SET created_at = ? WHERE source_event_id = ?;
`, late, loser.ID); err != nil {
		t.Fatal(err)
	}

	latest, err := st.LatestDraftSnapshot(ctx, project.ID)
	if err != nil {
		t.Fatalf("LatestDraftSnapshot: %v", err)
	}
	if latest == nil {
		t.Fatal("LatestDraftSnapshot returned nil")
	}
	if latest.SourceEventDeviceID != winner.DeviceID || latest.SourceEventID != winner.ID {
		t.Fatalf("latest = device=%q id=%q, want device=%q id=%q (canonical tiebreak; created_at/id would prefer the loser)",
			latest.SourceEventDeviceID, latest.SourceEventID, winner.DeviceID, winner.ID)
	}
	if latest.BlobRef != "age_blob:"+strings.Repeat("a", 64) {
		t.Fatalf("BlobRef = %q, want winner's blob", latest.BlobRef)
	}
}

// TestPruneDraftSnapshotsDeterministicTiebreak (P7-SYNC-03): on an HLC tie,
// PruneDraftSnapshots must keep the lexicographically-highest
// (source_event_device_id, source_event_id) and discard the rest — the same
// canonical ranking as LatestDraftSnapshot. Winner is inserted first with an
// earlier created_at so the old ORDER BY created_at DESC, id DESC would have
// kept the loser and pruned the winner.
func TestPruneDraftSnapshotsDeterministicTiebreak(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureWorkspace(ctx, "personal", "/tmp/Code"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureDevice(ctx, "test-device"); err != nil {
		t.Fatal(err)
	}
	project, err := st.UpsertProject(ctx, UpsertProjectParams{
		Path: "work/draft-prune-tie",
		Type: "draft_project",
	})
	if err != nil {
		t.Fatal(err)
	}

	const sameHLC int64 = 1_700_000_000_000
	// Canonical winner: higher device id (and higher event id) — insert first.
	winner := Event{ID: "evt_z_winner", DeviceID: "device_z", HLC: sameHLC}
	// Canonical loser: lower device id — insert second so local wall-clock/
	// snap id would win under the old ORDER BY created_at DESC, id DESC.
	loser := Event{ID: "evt_a_loser", DeviceID: "device_a", HLC: sameHLC}

	if err := st.RecordDraftSnapshot(ctx, project.ID, "age_blob:"+strings.Repeat("a", 64), 10, 1, winner); err != nil {
		t.Fatalf("RecordDraftSnapshot winner: %v", err)
	}
	if err := st.RecordDraftSnapshot(ctx, project.ID, "age_blob:"+strings.Repeat("b", 64), 20, 2, loser); err != nil {
		t.Fatalf("RecordDraftSnapshot loser: %v", err)
	}

	// Force created_at so old ordering demonstrably prefers the loser even if
	// both RecordDraftSnapshot calls shared the same nanosecond timestamp.
	early := formatTimestamp(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	late := formatTimestamp(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	if _, err := st.db.ExecContext(ctx, `
UPDATE draft_snapshots SET created_at = ? WHERE source_event_id = ?;
`, early, winner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `
UPDATE draft_snapshots SET created_at = ? WHERE source_event_id = ?;
`, late, loser.ID); err != nil {
		t.Fatal(err)
	}

	pruned, err := st.PruneDraftSnapshots(ctx, 1)
	if err != nil {
		t.Fatalf("PruneDraftSnapshots: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("pruned = %d, want 1", pruned)
	}

	latest, err := st.LatestDraftSnapshot(ctx, project.ID)
	if err != nil {
		t.Fatalf("LatestDraftSnapshot: %v", err)
	}
	if latest == nil {
		t.Fatal("LatestDraftSnapshot returned nil after prune")
	}
	if latest.SourceEventDeviceID != winner.DeviceID || latest.SourceEventID != winner.ID {
		t.Fatalf("surviving = device=%q id=%q, want device=%q id=%q (prune and latest-selection must agree)",
			latest.SourceEventDeviceID, latest.SourceEventID, winner.DeviceID, winner.ID)
	}
	if latest.BlobRef != "age_blob:"+strings.Repeat("a", 64) {
		t.Fatalf("BlobRef = %q, want winner's blob", latest.BlobRef)
	}
}

// TestRetainedBlobRefsDeterministicTiebreakMatchesPrune (P7-SYNC-03): on an
// HLC tie, RetainedBlobRefs (the `hub gc --dry-run` preview) must rank the
// same canonical (source_event_device_id, source_event_id) winner that
// PruneDraftSnapshots actually keeps, not the locally-newest created_at/id
// row — otherwise a dry-run preview can name the opposite blob as retained
// from what the real prune run would keep.
func TestRetainedBlobRefsDeterministicTiebreakMatchesPrune(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureWorkspace(ctx, "personal", "/tmp/Code"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.EnsureDevice(ctx, "test-device"); err != nil {
		t.Fatal(err)
	}
	project, err := st.UpsertProject(ctx, UpsertProjectParams{
		Path: "work/draft-retained-tie",
		Type: "draft_project",
	})
	if err != nil {
		t.Fatal(err)
	}

	const sameHLC int64 = 1_700_000_000_000
	// Canonical winner: higher device id (and higher event id) — insert first.
	winner := Event{ID: "evt_z_winner", DeviceID: "device_z", HLC: sameHLC}
	// Canonical loser: lower device id — insert second so local wall-clock/
	// snap id would win under the old ORDER BY created_at DESC, id DESC.
	loser := Event{ID: "evt_a_loser", DeviceID: "device_a", HLC: sameHLC}

	winnerBlob := "age_blob:" + strings.Repeat("a", 64)
	loserBlob := "age_blob:" + strings.Repeat("b", 64)
	if err := st.RecordDraftSnapshot(ctx, project.ID, winnerBlob, 10, 1, winner); err != nil {
		t.Fatalf("RecordDraftSnapshot winner: %v", err)
	}
	if err := st.RecordDraftSnapshot(ctx, project.ID, loserBlob, 20, 2, loser); err != nil {
		t.Fatalf("RecordDraftSnapshot loser: %v", err)
	}

	// Force created_at so old ordering demonstrably prefers the loser even if
	// both RecordDraftSnapshot calls shared the same nanosecond timestamp.
	early := formatTimestamp(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	late := formatTimestamp(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	if _, err := st.db.ExecContext(ctx, `
UPDATE draft_snapshots SET created_at = ? WHERE source_event_id = ?;
`, early, winner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `
UPDATE draft_snapshots SET created_at = ? WHERE source_event_id = ?;
`, late, loser.ID); err != nil {
		t.Fatal(err)
	}

	retained, err := st.RetainedBlobRefs(ctx, 1)
	if err != nil {
		t.Fatalf("RetainedBlobRefs: %v", err)
	}
	if len(retained) != 1 || retained[0] != winnerBlob {
		t.Fatalf("RetainedBlobRefs = %v, want [%s] (dry-run preview must agree with the canonical prune winner)", retained, winnerBlob)
	}

	// Confirm the real prune run keeps the SAME blob the dry-run preview named.
	pruned, err := st.PruneDraftSnapshots(ctx, 1)
	if err != nil {
		t.Fatalf("PruneDraftSnapshots: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("pruned = %d, want 1", pruned)
	}
	latest, err := st.LatestDraftSnapshot(ctx, project.ID)
	if err != nil {
		t.Fatalf("LatestDraftSnapshot: %v", err)
	}
	if latest == nil || latest.BlobRef != winnerBlob {
		var gotBlob string
		if latest != nil {
			gotBlob = latest.BlobRef
		}
		t.Fatalf("surviving blob after prune = %q, want %q (must match the dry-run preview)", gotBlob, winnerBlob)
	}
}
