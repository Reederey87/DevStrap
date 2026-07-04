package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestEveryCommandIsDocumented is the SPEC-5 drift gate: it walks the live
// cobra command tree and fails if any command path is absent from the command
// inventories, so the hand-maintained command lists cannot silently drift from
// the binary. It runs as part of the normal test suite, so CI enforces it
// automatically.
func TestEveryCommandIsDocumented(t *testing.T) {
	specPaths := []string{
		filepath.Join("..", "..", "spec", "13_CLI_DAEMON_API.md"),
		filepath.Join("..", "..", "spec", "00_START_HERE.md"),
	}
	specs := make(map[string]string, len(specPaths))
	for _, specPath := range specPaths {
		raw, err := os.ReadFile(specPath)
		if err != nil {
			t.Fatalf("read %s: %v", specPath, err)
		}
		specs[filepath.ToSlash(specPath)] = string(raw)
	}

	root := NewRootCommand(os.Stdout, os.Stderr)
	paths := collectCommandPaths(root)

	// Keep the matching rule intentionally simple: each visible Cobra command
	// path must appear as a contiguous substring in both spec inventories.
	for _, path := range paths {
		for specPath, specText := range specs {
			if !strings.Contains(specText, path) {
				t.Errorf("command %q is registered but not documented in %s", path, specPath)
			}
		}
	}
}

func collectCommandPaths(root *cobra.Command) []string {
	var paths []string
	collectCommandPathsFrom(root, "", &paths)
	return paths
}

func collectCommandPathsFrom(cmd *cobra.Command, parent string, out *[]string) {
	for _, sub := range cmd.Commands() {
		if sub.Hidden {
			continue
		}
		path := sub.Name()
		if parent != "" {
			path = parent + " " + path
		}
		*out = append(*out, path)
		collectCommandPathsFrom(sub, path, out)
	}
}
