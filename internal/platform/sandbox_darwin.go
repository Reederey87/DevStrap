//go:build darwin

package platform

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

const sandboxExecPath = "/usr/bin/sandbox-exec"

// SeatbeltSandbox confines agent children with macOS Seatbelt via
// /usr/bin/sandbox-exec (AGEN-03 first slice). sandbox-exec is deprecated by
// Apple but ships on every macOS release and is what current agent harnesses
// (Claude Code, Codex CLI, VT Code) use; if Apple ever removes it,
// Available() starts failing and `auto` mode degrades to a loud warning
// instead of breaking agent runs.
type SeatbeltSandbox struct{}

func (s SeatbeltSandbox) Name() string { return "seatbelt" }

func (s SeatbeltSandbox) Available() error {
	info, err := os.Stat(sandboxExecPath)
	if err != nil {
		return fmt.Errorf("%w: %s not found: %w", ErrUnsupported, sandboxExecPath, err)
	}
	if info.IsDir() || info.Mode()&0o111 == 0 {
		return fmt.Errorf("%w: %s is not executable", ErrUnsupported, sandboxExecPath)
	}
	return nil
}

func (s SeatbeltSandbox) Command(_ context.Context, spec SandboxSpec, argv []string) ([]string, func(), error) {
	if len(argv) == 0 {
		return nil, func() {}, fmt.Errorf("seatbelt: empty argv")
	}
	if err := s.Available(); err != nil {
		return nil, func() {}, err
	}
	resolved, err := resolveSandboxSpecPaths(spec)
	if err != nil {
		return nil, func() {}, err
	}
	// The profile lives in the run's log dir (0700) with the same 0600 mode
	// as the agent log; the caller removes it via cleanup after the child
	// exits.
	profilePath := filepath.Join(resolved.LogDir, "sandbox-"+filepath.Base(resolved.WorktreeDir)+".sb")
	if err := os.WriteFile(profilePath, []byte(sbplProfile(resolved)), 0o600); err != nil {
		return nil, func() {}, fmt.Errorf("write seatbelt profile: %w", err)
	}
	cleanup := func() { _ = os.Remove(profilePath) }
	wrapped := append([]string{sandboxExecPath, "-f", profilePath}, argv...)
	return wrapped, cleanup, nil
}
