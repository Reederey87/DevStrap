package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMigrationsDocumented (P5-DX-02) is a content-staleness gate: every
// migration file under internal/state/migrations must be named in
// spec/12_DATA_MODEL_SQLITE.md. Unlike the spec-drift file-touch gate, this
// inspects spec content, so a new migration cannot ship while the data-model
// spec's inventory silently goes stale (the 00010 collision this finding fixed).
func TestMigrationsDocumented(t *testing.T) {
	specPath := filepath.Join("..", "..", "spec", "12_DATA_MODEL_SQLITE.md")
	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read spec/12: %v", err)
	}
	spec := string(raw)

	migDir := filepath.Join("..", "state", "migrations")
	entries, err := os.ReadDir(migDir)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	found := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		found++
		if !strings.Contains(spec, name) {
			t.Errorf("migration %q exists but is not documented in spec/12_DATA_MODEL_SQLITE.md", name)
		}
	}
	if found == 0 {
		t.Fatal("no migration files found; the inventory check would be vacuous")
	}
}
