package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestEveryCommandIsDocumented is the SPEC-5 drift gate: it walks the live
// cobra command tree and fails if any command name is absent from
// spec/13_CLI_DAEMON_API.md, so the hand-maintained command list cannot
// silently drift from the binary. It runs as part of the normal test suite, so
// CI enforces it automatically.
func TestEveryCommandIsDocumented(t *testing.T) {
	specPath := filepath.Join("..", "..", "spec", "13_CLI_DAEMON_API.md")
	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read spec/13: %v", err)
	}
	spec := string(raw)

	root := NewRootCommand(os.Stdout, os.Stderr)
	var names []string
	collectCommandNames(root, &names)

	for _, name := range names {
		if name == "devstrap" || name == "help" || name == "completion" {
			continue
		}
		if !strings.Contains(spec, name) {
			t.Errorf("command %q is registered but not documented in spec/13_CLI_DAEMON_API.md", name)
		}
	}
}

func collectCommandNames(cmd *cobra.Command, out *[]string) {
	for _, sub := range cmd.Commands() {
		if sub.Hidden {
			continue
		}
		*out = append(*out, sub.Name())
		collectCommandNames(sub, out)
	}
}
