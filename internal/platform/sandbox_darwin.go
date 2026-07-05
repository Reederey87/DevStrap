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

func (s SeatbeltSandbox) Command(_ context.Context, spec SandboxSpec, argv []string) (SandboxCommand, error) {
	if len(argv) == 0 {
		return SandboxCommand{Cleanup: func() {}}, fmt.Errorf("seatbelt: empty argv")
	}
	if err := s.Available(); err != nil {
		return SandboxCommand{Cleanup: func() {}}, err
	}
	resolved, err := resolveSandboxSpecPaths(spec)
	if err != nil {
		return SandboxCommand{Cleanup: func() {}}, err
	}
	// Derive the credential deny anchors from the same lists as bubblewrap,
	// then resolve leaf symlinks: Seatbelt matches the kernel-real path, so a
	// deny on the literal ~/.ssh never fires when ~/.ssh is itself a symlink.
	// (bwrapSensitivePaths returns nil,nil unless DenySensitiveReads.)
	denyDirs, denyFiles := bwrapSensitivePaths(resolved)
	denyDirs = seatbeltDenyPaths(denyDirs)
	denyFiles = seatbeltDenyPaths(denyFiles)
	// The profile lives in the run's log dir (0700) with the same 0600 mode
	// as the agent log; the caller removes it via cleanup after the child
	// exits.
	profilePath := filepath.Join(resolved.LogDir, "sandbox-"+filepath.Base(resolved.WorktreeDir)+".sb")
	if err := os.WriteFile(profilePath, []byte(sbplProfile(resolved, denyDirs, denyFiles)), 0o600); err != nil {
		return SandboxCommand{Cleanup: func() {}}, fmt.Errorf("write seatbelt profile: %w", err)
	}
	cleanup := func() { _ = os.Remove(profilePath) }
	// Seatbelt has no seccomp analogue, so DenyDangerousSyscalls is a no-op
	// here and no ExtraFiles are needed.
	wrapped := append([]string{sandboxExecPath, "-f", profilePath}, argv...)
	return SandboxCommand{Argv: wrapped, Cleanup: cleanup}, nil
}

// seatbeltDenyPaths returns the deduped union of each raw credential path and
// its symlink-resolved target. Seatbelt is allow-default, so denying BOTH the
// literal alias and its resolved target is safe and closes the hole where e.g.
// ~/.ssh is itself a symlink to /elsewhere — a reader referencing either name
// is denied.
//
// This is deliberately STRONGER than reusing existingRealPaths verbatim: bwrap
// mounts over the dest, so it uses only the resolved target and must DROP absent
// paths (mounting over a missing dest errors). A Seatbelt deny RULE is harmless
// on an absent or literal path, so we never drop — an absent credential dir
// keeps its literal deny, and an unresolvable-but-present path keeps at least
// its literal deny. The resolved half comes from the shared fail-closed
// existingRealPaths (which drops absent and keeps unresolvable literals); the
// raw half guarantees every literal alias is denied regardless.
func seatbeltDenyPaths(raw []string) []string {
	seen := make(map[string]struct{}, len(raw)*2)
	var out []string
	add := func(p string) {
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	// Literal aliases first (deterministic, and always denied), then the
	// resolved targets contributed by the shared resolver.
	for _, p := range raw {
		add(p)
	}
	for _, p := range existingRealPaths(raw) {
		add(p)
	}
	return out
}
