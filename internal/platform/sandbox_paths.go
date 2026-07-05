package platform

import (
	"fmt"
	"path/filepath"
)

// resolveSandboxSpecPaths symlink-resolves every path in the spec. Seatbelt
// and bubblewrap both match kernel-real paths: Seatbelt rules are checked
// against resolved targets, and bwrap mounts land on resolved targets. /tmp,
// TMPDIR, and cloud-drive roots are routinely symlinks on Linux too, so an
// unresolved allow-list would silently confine nothing (or worse, deny the
// temp dir and break the child).
func resolveSandboxSpecPaths(spec SandboxSpec) (SandboxSpec, error) {
	out := spec
	for name, field := range map[string]*string{
		"worktree": &out.WorktreeDir,
		"tmp":      &out.TmpDir,
		"log":      &out.LogDir,
	} {
		if *field == "" {
			continue
		}
		real, err := filepath.EvalSymlinks(*field)
		if err != nil {
			return SandboxSpec{}, fmt.Errorf("resolve sandbox %s dir %q: %w", name, *field, err)
		}
		*field = real
	}
	// Deny anchors may legitimately not exist (no ~/.kube); resolve when
	// possible, keep the literal path otherwise — a deny on a nonexistent
	// path is harmless.
	for _, field := range []*string{&out.UserHome, &out.DevstrapHome} {
		if *field == "" {
			continue
		}
		if real, err := filepath.EvalSymlinks(*field); err == nil {
			*field = real
		}
	}
	return out, nil
}
