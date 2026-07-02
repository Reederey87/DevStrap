package cli

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/state"
)

func TestDraftSnapshotCreateRecordsOriginSnapshotRow(t *testing.T) {
	ctx := context.Background()
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	local := filepath.Join(root, "work", "local")
	runGit(t, local, "init")
	if err := os.WriteFile(filepath.Join(local, "note.txt"), []byte("draft\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := executeForTest("--home", home, "--root", root, "scan", "--adopt"); err != nil {
		t.Fatalf("scan stderr = %q err = %v", stderr, err)
	}

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "draft", "snapshot", "create", "work/local")
	if err != nil {
		t.Fatalf("draft snapshot create stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	if !strings.Contains(stdout, "Created draft snapshot") || !strings.Contains(stdout, "age_blob:") {
		t.Fatalf("stdout = %q, want created snapshot with blob ref", stdout)
	}

	store, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(store)
	project, err := store.ProjectByPath(ctx, "work/local")
	if err != nil {
		t.Fatal(err)
	}
	latest, err := store.LatestDraftSnapshot(ctx, project.ID)
	if err != nil {
		t.Fatalf("LatestDraftSnapshot: %v", err)
	}
	if latest == nil {
		t.Fatal("LatestDraftSnapshot is nil immediately after draft snapshot create")
	}
	refs, err := store.RetainedBlobRefs(ctx, 1)
	if err != nil {
		t.Fatalf("RetainedBlobRefs: %v", err)
	}
	if !slices.Contains(refs, latest.BlobRef) {
		t.Fatalf("RetainedBlobRefs = %v, want %s", refs, latest.BlobRef)
	}
}
