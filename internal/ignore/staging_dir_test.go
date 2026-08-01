package ignore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStagingDirPrunedUsingTheRealCreatedName is the anti-drift guard.
//
// It does NOT hardcode a staging-directory name. It builds one exactly the way
// cloneTempDir does — os.MkdirTemp(parent, "."+base+StagingDirMarker+"*") —
// and asserts the scanner prunes whatever that actually produces. If the
// creation formula ever changes without the pattern following, this fails.
//
// A test asserting a hand-written literal would keep passing while the real
// staging dirs became adoptable again, which is precisely the defect.
func TestStagingDirPrunedUsingTheRealCreatedName(t *testing.T) {
	parent := t.TempDir()
	created, err := os.MkdirTemp(parent, "."+"api-server"+StagingDirMarker+"*")
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(created)
	if !strings.Contains(name, StagingDirMarker) {
		t.Fatalf("fixture is vacuous: %q does not carry the marker, so this proves nothing", name)
	}

	m := DefaultMatcher()
	if !m.ShouldPruneDir(name, "work/acme/"+name) {
		t.Fatalf("ShouldPruneDir(%q) = false; an unpruned staging dir is walked by scan, "+
			"adopted as a second project carrying the real remote, and replicated fleet-wide", name)
	}
}

// TestStagingPruneDoesNotSwallowOrdinaryDirectories pins the other half: the
// pattern must be narrow. Pruning every dot-directory — or anything merely
// containing "tmp" — would hide real user content from the scanner, which is a
// worse failure than the one being fixed because it is silent.
func TestStagingPruneDoesNotSwallowOrdinaryDirectories(t *testing.T) {
	m := DefaultMatcher()
	for _, name := range []string{
		".config",
		".github",
		".vscode",
		"tmp",
		"temp",
		".hidden-project",
		"devstrap-tmp",         // no leading dot, no trailing dash-random
		"my.devstrap-tmpfiles", // marker requires the trailing hyphen
	} {
		if m.ShouldPruneDir(name, "work/acme/"+name) {
			t.Errorf("ShouldPruneDir(%q) = true; the staging pattern is too broad and is hiding real content", name)
		}
	}
}
