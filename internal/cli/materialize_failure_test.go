package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/redact"
	"github.com/Reederey87/DevStrap/internal/state"
)

// TestMaterializeFailurePersistsLastError pins P4-GIT-07: a failed materialize
// records scrubbed error text in device_project_state.last_error, surfaces it
// via status/doctor, stays in SkeletonProjects for retry, and clears on success.
func TestMaterializeFailurePersistsLastError(t *testing.T) {
	t.Setenv("DEVSTRAP_NO_KEYCHAIN", "1")
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	nsPath := "work/acme/broken-remote"

	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}

	// Unreachable file:// remote: clone fails deterministically without network.
	badRemote := "file://" + filepath.Join(t.TempDir(), "does-not-exist.git")
	if _, stderr, err := executeForTest(
		"--home", home, "--root", root,
		"add", badRemote, "--path", nsPath, "--default-branch", "main",
	); err != nil {
		t.Fatalf("add stderr = %q err = %v", stderr, err)
	}

	// (failure) materialize should fail and persist last_error.
	if _, _, err := executeForTest("--home", home, "--root", root, "materialize", nsPath); err == nil {
		t.Fatal("expected materialize to fail against unreachable remote")
	}

	ctx := context.Background()
	opts := testOptions(home, root)
	store, err := opts.openState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(store)

	// (a) last_error is queryable and scrub-stable (no secret-shaped text to strip).
	project, err := store.ProjectByPath(ctx, nsPath)
	if err != nil {
		t.Fatal(err)
	}
	if project.MaterializationState != "failed" {
		t.Fatalf("MaterializationState = %q, want failed", project.MaterializationState)
	}
	if project.LastError == "" {
		t.Fatal("LastError empty after failed materialize")
	}
	if !strings.HasPrefix(project.LastError, "clone: ") {
		t.Fatalf("LastError = %q, want clone: prefix", project.LastError)
	}
	if scrubbed := redact.Scrub(project.LastError); scrubbed != project.LastError {
		t.Fatalf("LastError was not scrub-stable: got %q after Scrub, want %q", scrubbed, project.LastError)
	}
	if strings.Contains(project.LastError, redact.Placeholder) {
		t.Fatalf("LastError contains redaction placeholder unexpectedly: %q", project.LastError)
	}

	// (e) SkeletonProjects still includes the failed project (resume path intact).
	skeleton, err := store.SkeletonProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range skeleton {
		if p.Path == nsPath {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("SkeletonProjects missing failed project %q (got %d projects)", nsPath, len(skeleton))
	}
	closeStore(store)
	store = nil

	// (b) status --json includes last_error for the project.
	statusOut, statusErr, err := executeForTest("--home", home, "--root", root, "status", "--json")
	if err != nil {
		t.Fatalf("status --json stderr = %q err = %v", statusErr, err)
	}
	var summary state.Summary
	if err := json.Unmarshal([]byte(statusOut), &summary); err != nil {
		t.Fatalf("status --json unmarshal: %v\n%s", err, statusOut)
	}
	var statusProject *state.ProjectStatus
	for i := range summary.Projects {
		if summary.Projects[i].Path == nsPath {
			statusProject = &summary.Projects[i]
			break
		}
	}
	if statusProject == nil {
		t.Fatalf("status --json missing project %q", nsPath)
	}
	if statusProject.LastError == "" {
		t.Fatal("status --json last_error empty")
	}
	if statusProject.LastError != project.LastError {
		t.Fatalf("status last_error = %q, want %q", statusProject.LastError, project.LastError)
	}

	// (c) doctor --json includes a warning check for the failed project.
	doctorOut, doctorErr, err := executeForTest("--home", home, "--root", root, "doctor", "--json")
	if err != nil {
		t.Fatalf("doctor --json stderr = %q err = %v", doctorErr, err)
	}
	var checks []checkResult
	if err := json.Unmarshal([]byte(doctorOut), &checks); err != nil {
		t.Fatalf("doctor --json unmarshal: %v\n%s", err, doctorOut)
	}
	wantName := "materialize: " + nsPath
	var matCheck *checkResult
	for i := range checks {
		if checks[i].Name == wantName {
			matCheck = &checks[i]
			break
		}
	}
	if matCheck == nil {
		t.Fatalf("doctor --json missing check %q among %d results", wantName, len(checks))
	}
	if matCheck.Status != checkWarn {
		t.Fatalf("doctor check status = %q, want %q", matCheck.Status, checkWarn)
	}
	if matCheck.Detail == "" {
		t.Fatal("doctor check detail empty")
	}
	if !strings.Contains(matCheck.Remedy, "materialize") {
		t.Fatalf("doctor remedy = %q, want materialize/sync retry hint", matCheck.Remedy)
	}

	// Fix the remote to a real bare repo and re-materialize successfully.
	goodRemote := createHydrateRemote(t, false)
	store, err = opts.openState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertProject(ctx, state.UpsertProjectParams{
		Path:                 nsPath,
		Type:                 "git_repo",
		RemoteURL:            "file://" + goodRemote,
		RemoteKey:            "local/broken-remote",
		DefaultBranch:        "main",
		MaterializationState: "failed",
		DirtyState:           "unknown",
	}); err != nil {
		closeStore(store)
		t.Fatalf("UpsertProject fix remote: %v", err)
	}
	closeStore(store)

	// Ensure no leftover skeleton/partial checkout blocks the success path.
	localPath := filepath.Join(root, filepath.FromSlash(nsPath))
	_ = os.RemoveAll(localPath)

	if stdout, stderr, err := executeForTest("--home", home, "--root", root, "materialize", nsPath); err != nil {
		t.Fatalf("successful materialize failed: stdout=%q stderr=%q err=%v", stdout, stderr, err)
	}

	// (d) self-healing: successful materialize clears last_error.
	store, err = opts.openState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(store)
	fixed, err := store.ProjectByPath(ctx, nsPath)
	if err != nil {
		t.Fatal(err)
	}
	if fixed.MaterializationState != "available" {
		t.Fatalf("after success MaterializationState = %q, want available", fixed.MaterializationState)
	}
	if fixed.LastError != "" {
		t.Fatalf("after success LastError = %q, want empty (self-heal)", fixed.LastError)
	}
}
