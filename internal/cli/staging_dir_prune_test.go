package cli

import (
	"path/filepath"
	"testing"

	"github.com/Reederey87/DevStrap/internal/ignore"
)

// TestCloneTempDirIsPrunedByTheScanner is the anti-drift guard that matters:
// it calls the REAL cloneTempDir and asserts the scanner prunes what it
// actually creates.
//
// The ignore-package test builds an equivalent name from the same formula;
// this one removes even that duplication. If cloneTempDir's naming ever
// changes without the ignore pattern following, an orphan left by a killed
// clone becomes adoptable again — as a second project carrying the real
// remote, replicated fleet-wide. That is the defect W12-01 fixed, and this is
// the test that keeps it fixed.
func TestCloneTempDirIsPrunedByTheScanner(t *testing.T) {
	target := filepath.Join(t.TempDir(), "work", "acme", "api-server")

	tmpPath, err := cloneTempDir(target)
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(tmpPath)

	if !ignore.DefaultMatcher().ShouldPruneDir(name, "work/acme/"+name) {
		t.Fatalf("cloneTempDir created %q, which the scanner does NOT prune. "+
			"An orphan of this shape is walked by `scan --adopt`, adopted as a second project "+
			"sharing the real remote, and replicated to every device.", name)
	}
}
