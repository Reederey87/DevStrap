package platform

import (
	"errors"
	"os"
	"path/filepath"
)

// existingRealPaths resolves each credential path to its symlink-real target,
// dropping only genuinely absent paths. It is the shared fail-closed resolver
// for both the bubblewrap (Linux) and Seatbelt (darwin) adapters, so the rule
// lives in exactly one place — build-tag-free rather than under //go:build
// linux — and cannot drift between the two.
//
// Fail-closed contract: a symlink resolving to its real target is masked at the
// target; a path that exists but cannot be resolved (permission denied, symlink
// loop, I/O error) keeps its literal path — dropping it would leave a
// credential readable. Only os.ErrNotExist is dropped: an absent credential
// path has nothing to mask.
//
// bwrap MOUNTS over these dests (so it needs the resolved target and must drop
// absent ones); Seatbelt only emits deny RULES, so its adapter additionally
// keeps the literal alias — see seatbeltDenyPaths.
func existingRealPaths(paths []string) []string {
	var out []string
	for _, path := range paths {
		// Mount over the REAL target: mounting over a symlink lands on its
		// target, and ~/.ssh -> elsewhere must mask elsewhere.
		real, err := filepath.EvalSymlinks(path)
		if err == nil {
			out = append(out, real)
			continue
		}
		// A genuinely absent credential path has nothing to mask, and bwrap
		// would fail with "Can't mkdir" if asked to mount over a missing dest
		// under the read-only root, so drop it. But EvalSymlinks also fails on
		// permission-denied, symlink loops, and I/O errors — for a mask that
		// backs DenySensitiveReads, silently dropping any of THOSE would leave
		// the credential path readable. Fail closed instead: keep the literal
		// path (bwrap resolves the mount dest itself, so masking the symlink
		// still masks its target; if the dest truly cannot be mounted the run
		// errors rather than proceeding with the credential exposed).
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		out = append(out, path)
	}
	return out
}
