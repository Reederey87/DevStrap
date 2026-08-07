package git

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Cone-mode sparse-checkout profiles (W12-02). This narrows the WORKING TREE
// of an already-cloned/checked-out repo — it is the working-tree/index-cost
// reduction that complements the existing blobless partial clone's
// transfer-cost reduction (see spec/08_GIT_MATERIALIZATION_AND_WORKTREES.md
// for the full distinction against internal/ignore's .devstrapignore
// compiler, which controls sync/materialization INCLUSION universally,
// BEFORE anything is cloned).
//
// Cone mode is the only supported mode: git's own documentation deprecates
// non-cone (pattern) mode — O(N×M) pattern-matching cost, and it is
// incompatible with --sparse-index. ValidSparsePath is the enforcement point
// that rejects anything that is not a plain, repo-relative directory path.

// SparseCheckoutInit enables cone-mode sparse-checkout on dir
// (`git sparse-checkout init --cone`). cone must be true — cone mode is the
// only supported mode (see package doc above) — so a caller passing false
// gets a clear rejection here instead of this ever shelling out
// `git sparse-checkout init` without --cone.
func (r Runner) SparseCheckoutInit(ctx context.Context, dir string, cone bool) error {
	if !cone {
		return errors.New("only cone-mode sparse-checkout is supported (git deprecates non-cone/pattern mode); pass cone=true")
	}
	_, err := r.Run(ctx, dir, "sparse-checkout", "init", "--cone")
	return err
}

// SparseCheckoutSet replaces dir's cone-mode sparse-checkout set with exactly
// paths (`git sparse-checkout set -- <paths...>`). Cone semantics: git also
// always includes root-level files and each leading directory's own
// immediate files. Every path is validated with ValidSparsePath before it
// reaches argv — cone mode accepts only plain repo-relative directory paths,
// never non-cone glob patterns — and "--" terminates option parsing so a
// validated-but-adversarial path can never be read as a git flag.
func (r Runner) SparseCheckoutSet(ctx context.Context, dir string, paths []string) error {
	if len(paths) == 0 {
		return errors.New("sparse-checkout set requires at least one path")
	}
	args := make([]string, 0, len(paths)+3)
	args = append(args, "sparse-checkout", "set", "--")
	for _, p := range paths {
		if err := ValidSparsePath(p); err != nil {
			return err
		}
		args = append(args, p)
	}
	_, err := r.Run(ctx, dir, args...)
	return err
}

// SparseCheckoutList reads dir's currently active cone-mode directories
// (`git sparse-checkout list`). It exists so callers can converge
// idempotently: re-applying an already-matching profile on every
// sync/hydrate would otherwise churn a git subprocess call for no effect. A
// worktree that has never had sparse-checkout enabled reports "not sparse"
// on stderr and a non-zero exit; that specific, well-known shape is treated
// as an empty set rather than an error, since "sparse-checkout was never
// enabled" and "sparse-checkout is enabled with zero directories" are the
// same starting point for convergence purposes.
func (r Runner) SparseCheckoutList(ctx context.Context, dir string) ([]string, error) {
	out, err := r.Run(ctx, dir, "sparse-checkout", "list")
	if err != nil {
		var cmdErr CommandError
		if errors.As(err, &cmdErr) && strings.Contains(strings.ToLower(cmdErr.Message), "not sparse") {
			return nil, nil
		}
		return nil, err
	}
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			paths = append(paths, line)
		}
	}
	return paths, nil
}

// SparseCheckoutDisable turns off sparse-checkout on dir
// (`git sparse-checkout disable`), restoring a full working tree. Used both
// to clear a project's sparse profile back to a full checkout, and as a
// best-effort rollback when SparseCheckoutSet fails after SparseCheckoutInit
// already succeeded — leaving a tree narrowed to just cone mode's
// always-included root files (a near-empty checkout) would be worse than the
// full tree the caller started with. A dir that is not currently sparse
// disables as a no-op (git exits 0).
func (r Runner) SparseCheckoutDisable(ctx context.Context, dir string) error {
	_, err := r.Run(ctx, dir, "sparse-checkout", "disable")
	return err
}

// ApplyConvergedSparseCheckout enables cone-mode sparse-checkout on dir and
// sets it to exactly paths, first checking SparseCheckoutList so an
// already-converged tree is left untouched (no subprocess churn on an idle
// sync/hydrate). paths must be non-empty; callers that want to clear a
// profile back to a full checkout use SparseCheckoutDisable directly.
//
// Every path is validated BEFORE any git mutation runs (Codex review,
// W12-02): SparseCheckoutInit narrows a never-sparse repo to cone mode's
// top-level-only files as a SIDE EFFECT of enabling it, so validating deep
// inside SparseCheckoutSet — which used to run only after Init had already
// mutated the tree — left a window where a single invalid path narrowed the
// tree before the caller ever found out the request would fail.
//
// On a genuine SparseCheckoutSet failure (a validation-passing path git
// itself rejects) after a successful Init, this restores dir's PRIOR active
// cone (the current snapshot read above) rather than unconditionally
// disabling sparse-checkout: if dir already had a working profile before
// this call, falling back to a full checkout would be a WORSE outcome than
// the state the caller started with. Disable is used only when dir had no
// prior profile (current is empty) or the restore attempt itself fails.
func (r Runner) ApplyConvergedSparseCheckout(ctx context.Context, dir string, paths []string) error {
	if len(paths) == 0 {
		return errors.New("ApplyConvergedSparseCheckout requires at least one path")
	}
	for _, p := range paths {
		if err := ValidSparsePath(p); err != nil {
			return err
		}
	}
	current, err := r.SparseCheckoutList(ctx, dir)
	if err != nil {
		return fmt.Errorf("read current sparse-checkout state: %w", err)
	}
	if sparsePathSetsEqual(current, paths) {
		return nil
	}
	if err := r.SparseCheckoutInit(ctx, dir, true); err != nil {
		return fmt.Errorf("sparse-checkout init: %w", err)
	}
	if err := r.SparseCheckoutSet(ctx, dir, paths); err != nil {
		if len(current) > 0 {
			if restoreErr := r.SparseCheckoutSet(ctx, dir, current); restoreErr != nil {
				_ = r.SparseCheckoutDisable(ctx, dir)
			}
		} else {
			_ = r.SparseCheckoutDisable(ctx, dir)
		}
		return fmt.Errorf("sparse-checkout set: %w", err)
	}
	return nil
}

func sparsePathSetsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ac := append([]string(nil), a...)
	bc := append([]string(nil), b...)
	sort.Strings(ac)
	sort.Strings(bc)
	for i := range ac {
		if ac[i] != bc[i] {
			return false
		}
	}
	return true
}

// CleanSparsePath normalizes a cone-mode sparse-checkout directory path for
// storage and comparison: it trims surrounding whitespace and ALL trailing
// slashes (not just one — Codex review, W12-02: stripping a single trailing
// slash left "src//" cleaned to "src/" instead of "src", storing a value
// that could never string-equal SparseCheckoutList's un-slashed "src" output
// and permanently defeating ApplyConvergedSparseCheckout's no-op-when-
// converged check for that entry), so "src/", "src//", "src", and " src "
// all store/compare identically. Callers clean a user-supplied path before
// validating and storing it.
func CleanSparsePath(path string) string {
	return strings.TrimRight(strings.TrimSpace(path), "/")
}

// ValidSparsePath reports whether path is a safe, plain cone-mode
// sparse-checkout directory path: repo-relative, slash-separated, with no
// glob metacharacters, gitignore-style negation/comment prefixes, "..",
// empty segments, or leading "-" (option injection). Cone mode accepts only
// directories, never git's non-cone glob patterns, so this validator is the
// hard-requirement enforcement point (see package doc above).
func ValidSparsePath(path string) error {
	if path == "" {
		return errors.New("sparse path must not be empty")
	}
	if strings.ContainsAny(path, " \t\n\r") {
		return fmt.Errorf("sparse path %q must not contain whitespace", path)
	}
	if strings.HasPrefix(path, "-") {
		return fmt.Errorf("sparse path %q must not start with '-'", path)
	}
	if strings.HasPrefix(path, "!") || strings.HasPrefix(path, "#") {
		return fmt.Errorf("sparse path %q must not start with '!' or '#' (gitignore-style negation/comment syntax has no meaning for a cone-mode directory path)", path)
	}
	if strings.HasPrefix(path, "/") {
		return fmt.Errorf("sparse path %q must be repo-relative, not absolute", path)
	}
	if strings.ContainsAny(path, "*?[]\\") {
		return fmt.Errorf("sparse path %q looks like a non-cone glob pattern; only plain directory paths are supported (cone mode)", path)
	}
	if strings.Contains(path, ":") {
		return fmt.Errorf("sparse path %q must not contain ':' (e.g. a Windows drive-qualified path)", path)
	}
	if strings.Contains(path, "//") {
		return fmt.Errorf("sparse path %q must not contain an empty path segment", path)
	}
	trimmed := strings.TrimSuffix(path, "/")
	if trimmed == "" {
		return fmt.Errorf("sparse path %q must not be empty", path)
	}
	for _, part := range strings.Split(trimmed, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("sparse path %q contains an invalid path segment", path)
		}
	}
	return nil
}
