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
	if version != 10 {
		t.Fatalf("schema version = %d, want 10", version)
	}

	var tableCount int
	if err := st.db.QueryRow(`
SELECT COUNT(*)
FROM sqlite_master
WHERE type = 'table' AND name IN (
  'workspaces', 'devices', 'namespace_entries', 'git_repos', 'draft_projects',
  'device_project_state', 'env_profiles', 'secret_bindings', 'worktrees',
  'agent_runs', 'events', 'jobs', 'conflicts', 'sync_cursors', 'event_delivery',
  'hub_cursors', 'draft_snapshots'
)`).Scan(&tableCount); err != nil {
		t.Fatal(err)
	}
	if tableCount != 17 {
		t.Fatalf("table count = %d, want 17", tableCount)
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
	if version != 9 {
		t.Fatalf("schema version after down = %d, want 9", version)
	}
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	version, err = st.Version()
	if err != nil {
		t.Fatal(err)
	}
	if version != 10 {
		t.Fatalf("schema version after re-migrate = %d, want 10", version)
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
