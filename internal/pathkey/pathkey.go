package pathkey

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/text/unicode/norm"
)

var (
	ErrEmpty        = errors.New("namespace path must not be empty")
	ErrAbsolute     = errors.New("namespace path must be relative")
	ErrEscape       = errors.New("namespace path must not escape root")
	ErrEmptyPart    = errors.New("namespace path contains an empty segment")
	ErrPathConflict = errors.New("namespace path conflicts by case-folded key")
	// ErrDangling marks a symlink (or path) whose target does not exist. It is
	// distinct from ErrEscape so callers can warn (dangling) rather than block
	// (escape).
	ErrDangling = errors.New("symlink target does not exist")
)

type Path struct {
	Display string
	Key     string
}

func Clean(input string) (Path, error) {
	raw := norm.NFC.String(strings.TrimSpace(filepath.ToSlash(input)))
	if raw == "" || raw == "." {
		return Path{}, ErrEmpty
	}
	if strings.HasPrefix(raw, "/") || filepath.IsAbs(input) {
		return Path{}, ErrAbsolute
	}
	parts := strings.Split(raw, "/")
	for _, part := range parts {
		switch part {
		case "", ".":
			return Path{}, ErrEmptyPart
		case "..":
			return Path{}, ErrEscape
		}
	}
	clean := pathClean(raw)
	if clean == "." || strings.HasPrefix(clean, "../") || clean == ".." {
		return Path{}, ErrEscape
	}
	return Path{Display: clean, Key: strings.ToLower(clean)}, nil
}

func FromRoot(root, path string) (Path, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return Path{}, fmt.Errorf("relative path: %w", err)
	}
	return Clean(rel)
}

// CheckSymlinkWithinRoot resolves path through symlinks and verifies the real
// target stays within root. It returns typed errors so callers can react
// precisely: ErrEscape (target outside root — block), ErrDangling (target
// missing — warn), or a wrapped IO error. errors.Is works against the
// sentinels.
func CheckSymlinkWithinRoot(root, path string) error {
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve root symlinks: %w", err)
	}
	pathReal, err := filepath.EvalSymlinks(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrDangling, path)
		}
		return fmt.Errorf("resolve path symlinks: %w", err)
	}
	rel, err := filepath.Rel(rootReal, pathReal)
	if err != nil {
		return fmt.Errorf("relative symlink target: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("%w: %s -> %s", ErrEscape, path, pathReal)
	}
	return nil
}

// VerifyWithinRoot re-validates, at use time, that target resolves to a real
// path inside root. It closes the TOCTOU window between an earlier scan-time
// check and a later materialization (hydrate/worktree) by re-resolving symlinks
// immediately before use. Returns ErrEscape when target escapes root.
func VerifyWithinRoot(root, target string) error {
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve root symlinks: %w", err)
	}
	targetReal, err := filepath.EvalSymlinks(target)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// A not-yet-created target (e.g. a skeleton path about to be
			// cloned) is fine as long as its existing parent stays within root.
			return verifyParentWithinRoot(rootReal, target)
		}
		return fmt.Errorf("resolve target symlinks: %w", err)
	}
	return relWithinRoot(rootReal, targetReal)
}

func verifyParentWithinRoot(rootReal, target string) error {
	// Walk up to the nearest existing ancestor and verify it stays within root.
	// A nested target whose intermediate dirs do not exist yet (common on a
	// peer device before skeletons are reconciled) is legitimate as long as the
	// existing portion of the path does not escape via a symlink.
	dir := filepath.Dir(target)
	for {
		real, err := filepath.EvalSymlinks(dir)
		if err == nil {
			return relWithinRoot(rootReal, real)
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("resolve target parent symlinks: %w", err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return fmt.Errorf("resolve target parent symlinks: %w", err)
		}
		dir = parent
	}
}

func relWithinRoot(rootReal, targetReal string) error {
	rel, err := filepath.Rel(rootReal, targetReal)
	if err != nil {
		return fmt.Errorf("relative target: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("%w: %s -> %s", ErrEscape, targetReal, rootReal)
	}
	return nil
}

func DetectCaseConflicts(paths []Path) error {
	seen := map[string]string{}
	for _, p := range paths {
		if prev, ok := seen[p.Key]; ok && prev != p.Display {
			return fmt.Errorf("%w: %s and %s", ErrPathConflict, prev, p.Display)
		}
		seen[p.Key] = p.Display
	}
	return nil
}

func pathClean(p string) string {
	stack := make([]string, 0, strings.Count(p, "/")+1)
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			if len(stack) == 0 {
				return ".."
			}
			stack = stack[:len(stack)-1]
			continue
		}
		stack = append(stack, part)
	}
	if len(stack) == 0 {
		return "."
	}
	return strings.Join(stack, "/")
}
