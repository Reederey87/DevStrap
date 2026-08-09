package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// CheckoutHead populates dir's working tree from its already-resolved HEAD
// (`git checkout`). It is used after a no-checkout clone once any desired
// sparse-checkout cone has been configured.
//
// This runs in the LONG TRANSFER class, not the default 2m one (P6-GIT-01).
// On a blobless/partial clone (--filter=blob:none) the checkout IS the network
// transfer: git lazily fetches a blob for every file it writes into the working
// tree. Without --no-checkout that fetch happens inside `git clone`, which
// already runs under LongTimeout; splitting the checkout into its own command
// would otherwise move that same transfer under the 2m cap and time out
// materialization of exactly the large monorepos this sparse path exists to
// make cheaper — and here a checkout failure is fatal, so the repo would fail
// to materialize at all where it previously succeeded.
func (r Runner) CheckoutHead(ctx context.Context, dir string) error {
	attemptCtx, cancel := r.longTransferContext(ctx)
	defer cancel()
	_, err := r.Run(attemptCtx, dir, "checkout")
	return err
}

// SubmoduleUpdateInit materializes every configured submodule recursively.
// A clone with both --no-checkout and --recurse-submodules exits successfully
// without populating submodule working trees, so callers that explicitly
// check out later must run this after CheckoutHead. Submodules may require
// network transfers, so this uses the same long timeout class as clone.
func (r Runner) SubmoduleUpdateInit(ctx context.Context, dir string) error {
	attemptCtx, cancel := r.longTransferContext(ctx)
	defer cancel()
	_, err := r.Run(attemptCtx, dir, "submodule", "update", "--init", "--recursive")
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
	// Normalize BEFORE comparing/setting (review follow-up, W12-02): git's
	// own cone-mode collapses an overlapping set (e.g. ["backend",
	// "backend/deep"]) down to just its ancestors when reporting the active
	// set (SparseCheckoutList), so comparing the un-collapsed input against
	// that collapsed report never matches — every hydrate/sync would re-run
	// init+set forever for any project with overlapping configured paths.
	// Callers (parseSparseFlag/cleanSparseArgs) also normalize at write time
	// so what's stored matches what actually gets applied, but normalizing
	// again here makes convergence correct regardless of how paths arrived.
	paths = NormalizeSparsePaths(paths)
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

// NormalizeSparsePaths removes any path made redundant by an ancestor
// directory already present in the set (review follow-up, W12-02): git's
// cone-mode `sparse-checkout set` collapses ["backend", "backend/deep"] down
// to just ["backend"] when reporting the active set, since including a
// directory already implies everything beneath it. Comparing an
// un-collapsed desired set against SparseCheckoutList's always-collapsed
// report permanently defeats ApplyConvergedSparseCheckout's no-op check for
// any project with overlapping configured paths.
//
// Each path is cleaned (CleanSparsePath) internally rather than assuming the
// caller already did so (second review follow-up): an un-cleaned trailing
// slash — e.g. "backend/" alongside "backend/deep" — would otherwise never
// string-equal its own already-cleaned ancestor, reintroducing the exact
// forever-loop bug this function exists to close via a different string
// representation of the same overlap. Exact duplicates and proper
// descendants of another entry are dropped, preserving each surviving
// (cleaned) path's original relative order.
func NormalizeSparsePaths(paths []string) []string {
	cleaned := make([]string, len(paths))
	for i, p := range paths {
		cleaned[i] = CleanSparsePath(p)
	}
	sorted := append([]string(nil), cleaned...)
	sort.Strings(sorted)
	survives := make(map[string]bool, len(sorted))
	var kept []string
	for _, p := range sorted {
		redundant := false
		for _, k := range kept {
			if p == k || strings.HasPrefix(p, k+"/") {
				redundant = true
				break
			}
		}
		if !redundant {
			kept = append(kept, p)
			survives[p] = true
		}
	}
	result := make([]string, 0, len(kept))
	seen := make(map[string]bool, len(kept))
	for _, p := range cleaned {
		if survives[p] && !seen[p] {
			result = append(result, p)
			seen[p] = true
		}
	}
	return result
}

// SparseCheckoutEverEnabled reports, via a cheap local filesystem check (no
// git subprocess), whether dir's git-dir has ever had sparse-checkout
// enabled. Used to gate the hot-path convergence probe in callers like
// applyProjectSparseProfile (review follow-up, W12-02): calling
// SparseCheckoutList unconditionally on EVERY sync/hydrate for EVERY
// project — even the vast majority that have never touched the sparse
// feature at all — measured a real subprocess cost per project, breaking
// the feature's opt-in/zero-cost-when-unused design goal for every existing
// user. A dir whose sparse-checkout state can't be determined this way (an
// unusual .git layout) reports true, the safe direction: it costs one extra
// subprocess call rather than silently skipping a real leftover-cone
// convergence (see the bidirectional-convergence doc on
// applyProjectSparseProfile).
func SparseCheckoutEverEnabled(dir string) bool {
	gitDir := filepath.Join(dir, ".git")
	info, err := os.Stat(gitDir)
	if err != nil {
		return true
	}
	if !info.IsDir() {
		// A linked worktree's ".git" is a file containing "gitdir: <path>".
		// dir is always a project's own already-materialized local path
		// (never raw user/network input reaching this function directly),
		// same trust boundary as every other local filesystem call in this
		// package.
		//nolint:gosec // dir is a caller-controlled local repo path, not user/network input; see comment above.
		data, rerr := os.ReadFile(gitDir)
		if rerr != nil {
			return true
		}
		line := strings.TrimSpace(string(data))
		const prefix = "gitdir:"
		if !strings.HasPrefix(line, prefix) {
			return true
		}
		gitDir = strings.TrimSpace(strings.TrimPrefix(line, prefix))
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(dir, gitDir)
		}
	}
	//nolint:gosec // gitDir is resolved from a caller-controlled local repo path (a real ".git" dir or its own "gitdir:" pointer target), not user/network input.
	_, err = os.Stat(filepath.Join(gitDir, "info", "sparse-checkout"))
	return err == nil
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
