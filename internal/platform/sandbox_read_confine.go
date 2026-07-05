package platform

import (
	"path/filepath"
	"runtime"
	"strings"
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

// credentialAnchors returns the absolute credential paths the sandbox protects
// (the same set bwrapSensitivePaths masks): the home credential dirs/files and
// DevstrapHome/keys. Used to reject read-allow roots that would re-expose them.
func credentialAnchors(userHome, devstrapHome string) []string {
	var anchors []string
	if userHome != "" {
		for _, rel := range sensitiveHomeDirs {
			anchors = append(anchors, filepath.Join(userHome, rel))
		}
		for _, rel := range sensitiveHomeFiles {
			anchors = append(anchors, filepath.Join(userHome, rel))
		}
	}
	if devstrapHome != "" {
		anchors = append(anchors, filepath.Join(devstrapHome, "keys"))
	}
	return anchors
}

// pathsOverlap reports whether a and b are the same path or one contains the
// other (either could expose the other's contents). Both must be clean+abs.
func pathsOverlap(a, b string) bool {
	if a == b {
		return true
	}
	sep := string(filepath.Separator)
	// The filesystem root contains every path, but "/" + sep is "//", which is
	// never a prefix of a clean absolute path — special-case it so
	// `--read-allow /` (the ultimate footgun) is caught, not silently allowed.
	if a == sep || b == sep {
		return true
	}
	return strings.HasPrefix(a, b+sep) || strings.HasPrefix(b, a+sep)
}

// FirstReadAllowCredentialConflict returns the first --read-allow root that
// overlaps a protected credential path, or "" if none. Read confinement drops
// bwrap's credential masks (subsumed by the allow-list) and Landlock cannot
// subtract from an allowed root, so a read-allow root that contains or sits
// inside a credential path would silently re-expose it on those backends —
// this lets the CLI fail closed instead. Seatbelt would still deny it (its
// credential deny is emitted last), but the guard keeps all backends honest.
func FirstReadAllowCredentialConflict(userHome, devstrapHome string, readAllow []string) string {
	anchors := credentialAnchors(userHome, devstrapHome)
	for _, raw := range readAllow {
		if !filepath.IsAbs(raw) {
			continue
		}
		root := filepath.Clean(raw)
		for _, anchor := range anchors {
			if pathsOverlap(root, filepath.Clean(anchor)) {
				return raw
			}
		}
	}
	return ""
}
