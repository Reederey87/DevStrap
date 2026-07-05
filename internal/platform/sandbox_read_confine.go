package platform

import (
	"path/filepath"
	"runtime"
)

// readConfineHomeCaches are the $HOME toolchain/build caches kept readable
// under read-confinement so compilers and package managers keep working. The
// credential dirs (.ssh, .aws, .gnupg, .config/gh, .kube, .docker) are
// deliberately absent — they stay masked/denied on top — so no read root ever
// contains a credential path.
var readConfineHomeCaches = []string{
	".cache", "go", ".cargo", ".rustup", ".npm", ".nvm", ".pyenv", ".local",
}

// readConfineSystemRootsLinux is the set of OS toolchain/library/runtime roots
// a build needs on Linux. /proc, /sys, /dev are runtime surfaces the child
// already gets via bwrap's --proc/--dev; listing them keeps the Landlock and
// Seatbelt allow-lists honest about what stays readable.
var readConfineSystemRootsLinux = []string{
	"/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/etc",
	"/opt", "/var", "/run", "/proc", "/sys", "/dev", "/nix", "/snap",
}

// readConfineSystemRootsDarwin is the macOS equivalent. /etc, /var, /tmp all
// resolve into /private, which is listed.
var readConfineSystemRootsDarwin = []string{
	"/usr", "/bin", "/sbin", "/System", "/Library", "/private",
	"/opt", "/Applications", "/dev", "/nix",
}

// readConfineSystemRoots returns the current OS's system read roots. Kept a
// function (not a var) so the pure builder stays deterministic and testable on
// any host via runtime.GOOS.
func readConfineSystemRoots() []string {
	if runtime.GOOS == "darwin" {
		return readConfineSystemRootsDarwin
	}
	return readConfineSystemRootsLinux
}

// readConfineRoots returns the absolute read-allow roots for a spec, deduped
// and order-stable: the worktree and per-run tmp (already read-write), the
// running OS's system/toolchain roots, the $HOME build caches, and any
// absolute ReadAllowExtra. Build-tag-free and pure so the allow-list is
// golden-tested on every platform. Only called when spec.ReadConfine is set.
func readConfineRoots(spec SandboxSpec) []string {
	seen := make(map[string]struct{})
	var roots []string
	add := func(p string) {
		if p == "" || !filepath.IsAbs(p) {
			return
		}
		clean := filepath.Clean(p)
		if _, ok := seen[clean]; ok {
			return
		}
		seen[clean] = struct{}{}
		roots = append(roots, clean)
	}
	add(spec.WorktreeDir)
	add(spec.TmpDir)
	for _, root := range readConfineSystemRoots() {
		add(root)
	}
	if spec.UserHome != "" {
		for _, cache := range readConfineHomeCaches {
			add(filepath.Join(spec.UserHome, cache))
		}
	}
	for _, extra := range spec.ReadAllowExtra {
		add(extra)
	}
	return roots
}
