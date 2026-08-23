package state

import (
	"context"
	"path/filepath"
	"testing"
)

func setupSparseTestProject(t *testing.T) (*Store, NamespaceEntry) {
	t.Helper()
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
	project, err := st.UpsertProject(ctx, UpsertProjectParams{Path: "work/acme/monorepo", Type: "git_repo", RemoteURL: "git@example.com:acme/monorepo.git", RemoteKey: "example.com/acme/monorepo"})
	if err != nil {
		t.Fatal(err)
	}
	return st, project
}

func TestSparsePathsForProjectEmptyByDefault(t *testing.T) {
	st, project := setupSparseTestProject(t)
	paths, err := st.SparsePathsForProject(context.Background(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 0 {
		t.Fatalf("SparsePathsForProject = %v, want empty for an unconfigured project", paths)
	}
}

func TestReplaceSparsePathsForProjectRoundTrips(t *testing.T) {
	st, project := setupSparseTestProject(t)
	ctx := context.Background()

	if err := st.ReplaceSparsePathsForProject(ctx, project.ID, []string{"backend", "docs"}); err != nil {
		t.Fatal(err)
	}
	paths, err := st.SparsePathsForProject(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || paths[0] != "backend" || paths[1] != "docs" {
		t.Fatalf("SparsePathsForProject = %v, want [backend docs] (ordered)", paths)
	}

	// A second call REPLACES the set, not appends.
	if err := st.ReplaceSparsePathsForProject(ctx, project.ID, []string{"frontend"}); err != nil {
		t.Fatal(err)
	}
	paths, err = st.SparsePathsForProject(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != "frontend" {
		t.Fatalf("SparsePathsForProject after replace = %v, want [frontend]", paths)
	}
}

func TestReplaceSparsePathsForProjectEmptyClearsProfile(t *testing.T) {
	st, project := setupSparseTestProject(t)
	ctx := context.Background()
	if err := st.ReplaceSparsePathsForProject(ctx, project.ID, []string{"backend"}); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceSparsePathsForProject(ctx, project.ID, nil); err != nil {
		t.Fatal(err)
	}
	paths, err := st.SparsePathsForProject(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 0 {
		t.Fatalf("SparsePathsForProject after clearing = %v, want empty", paths)
	}
}

func TestSetSparsePathsTxWithinExistingTransaction(t *testing.T) {
	st, project := setupSparseTestProject(t)
	ctx := context.Background()
	if err := st.WithTx(ctx, func(tx *Tx) error {
		return tx.SetSparsePathsTx(ctx, project.ID, []string{"tools", "src"})
	}); err != nil {
		t.Fatal(err)
	}
	paths, err := st.SparsePathsForProject(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || paths[0] != "src" || paths[1] != "tools" {
		t.Fatalf("SparsePathsForProject = %v, want [src tools] (ordered, not insertion order)", paths)
	}
}

func TestSparsePathsScopedPerProject(t *testing.T) {
	st, project := setupSparseTestProject(t)
	ctx := context.Background()
	other, err := st.UpsertProject(ctx, UpsertProjectParams{Path: "work/acme/other", Type: "git_repo", RemoteURL: "git@example.com:acme/other.git", RemoteKey: "example.com/acme/other"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceSparsePathsForProject(ctx, project.ID, []string{"backend"}); err != nil {
		t.Fatal(err)
	}
	otherPaths, err := st.SparsePathsForProject(ctx, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(otherPaths) != 0 {
		t.Fatalf("unrelated project's sparse paths = %v, want empty (no cross-project leakage)", otherPaths)
	}
}

// TestProjectSparsePathsCascadeDeletesWithNamespaceEntry pins the migration's
// FOREIGN KEY(namespace_id) REFERENCES namespace_entries(id) ON DELETE
// CASCADE: a hard-deleted namespace_entries row (not the normal soft-delete
// tombstone path) must take its sparse-paths rows with it rather than
// leaving orphans.
func TestProjectSparsePathsCascadeDeletesWithNamespaceEntry(t *testing.T) {
	st, project := setupSparseTestProject(t)
	ctx := context.Background()
	if err := st.ReplaceSparsePathsForProject(ctx, project.ID, []string{"backend"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `DELETE FROM namespace_entries WHERE id = ?;`, project.ID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM project_sparse_paths WHERE namespace_id = ?;`, project.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("project_sparse_paths rows remaining after namespace_entries delete = %d, want 0 (ON DELETE CASCADE)", count)
	}
}

func TestSparseProbeCacheRoundTripsAndReplaces(t *testing.T) {
	st, project := setupSparseTestProject(t)
	ctx := context.Background()

	cache, found, err := st.SparseProbeCacheForProject(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatalf("unexpected default cache: %+v", cache)
	}

	first := SparseProbeCache{
		HeadSHA:      "1111111111111111111111111111111111111111",
		ProfileHash:  "profile-a",
		MissingPaths: []string{"bakend", "docz"},
	}
	if err := st.RecordSparseProbeCache(ctx, project.ID, first); err != nil {
		t.Fatal(err)
	}
	got, found, err := st.SparseProbeCacheForProject(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !found || got.HeadSHA != first.HeadSHA || got.ProfileHash != first.ProfileHash || len(got.MissingPaths) != 2 || got.MissingPaths[0] != "bakend" || got.MissingPaths[1] != "docz" {
		t.Fatalf("SparseProbeCacheForProject = %+v, found=%v; want %+v", got, found, first)
	}

	second := SparseProbeCache{
		HeadSHA:      "2222222222222222222222222222222222222222",
		ProfileHash:  "profile-b",
		MissingPaths: []string{},
	}
	if err := st.RecordSparseProbeCache(ctx, project.ID, second); err != nil {
		t.Fatal(err)
	}
	got, found, err = st.SparseProbeCacheForProject(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !found || got.HeadSHA != second.HeadSHA || got.ProfileHash != second.ProfileHash || len(got.MissingPaths) != 0 {
		t.Fatalf("replacement cache = %+v, found=%v; want %+v", got, found, second)
	}
}

func TestSparseProbeCacheCascadeDeletesWithNamespaceEntry(t *testing.T) {
	st, project := setupSparseTestProject(t)
	ctx := context.Background()
	if err := st.RecordSparseProbeCache(ctx, project.ID, SparseProbeCache{
		HeadSHA:     "1111111111111111111111111111111111111111",
		ProfileHash: "profile-a",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `DELETE FROM namespace_entries WHERE id = ?;`, project.ID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM project_sparse_probe_cache WHERE namespace_id = ?;`, project.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("project_sparse_probe_cache rows remaining after namespace_entries delete = %d, want 0", count)
	}
}

func TestSparseProbeCacheRejectsInvalidMissingPathsJSON(t *testing.T) {
	st, project := setupSparseTestProject(t)
	ctx := context.Background()
	if _, err := st.db.ExecContext(ctx, `
INSERT INTO project_sparse_probe_cache (
  namespace_id, head_sha, profile_hash, missing_paths_json, updated_at
) VALUES (?, ?, ?, ?, ?);
`, project.ID, "1111111111111111111111111111111111111111", "profile-a", "not-json", timestampNow()); err != nil {
		t.Fatal(err)
	}
	if _, found, err := st.SparseProbeCacheForProject(ctx, project.ID); err == nil || found {
		t.Fatalf("SparseProbeCacheForProject invalid JSON: found=%v err=%v, want an error and found=false", found, err)
	}
}
