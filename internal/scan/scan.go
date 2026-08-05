package scan

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	dsgit "github.com/Reederey87/DevStrap/internal/git"
	"github.com/Reederey87/DevStrap/internal/ignore"
	"github.com/Reederey87/DevStrap/internal/pathkey"
)

type Type string

const (
	TypeGitRepo     Type = "git_repo"
	TypeLocalGit    Type = "local_git" // NOVCS-01: git repo with no usable remote; never synced as clonable.
	TypeDraftFolder Type = "draft_project"
	TypePlainFolder Type = "plain_folder"
)

type Finding struct {
	Path          string   `json:"path"`
	Type          Type     `json:"type"`
	RemoteURL     string   `json:"remote_url,omitempty"`
	RemoteKey     string   `json:"remote_key,omitempty"`
	DefaultBranch string   `json:"default_branch,omitempty"`
	Warnings      []string `json:"warnings,omitempty"`
}

type Result struct {
	Root       string      `json:"root"`
	Findings   []Finding   `json:"findings"`
	Warnings   []string    `json:"warnings,omitempty"`
	Duplicates []Duplicate `json:"duplicates,omitempty"`
	// Secrets lists secret-looking files (relative slash paths) discovered
	// during the walk so callers can quarantine or ignore them.
	Secrets []string `json:"secrets,omitempty"`
	// PrunedDirs counts directories skipped by the ignore rules (defaults +
	// root .devstrapignore). Informational: the CLI surfaces it as progress
	// output, not a warning, so run-loop ticks stay quiet about routine prunes.
	PrunedDirs int
}

type Duplicate struct {
	RemoteKey       string   `json:"remote_key"`
	Paths           []string `json:"paths"`
	RecommendedPath string   `json:"recommended_path"`
}

type Options struct {
	// IncludeNonGit classifies directories that are not git repositories —
	// draft_project and plain_folder. Every production call site passes true;
	// false yields a git-only scan.
	IncludeNonGit bool
	Git           dsgit.Runner
	// Ignore overrides the compiled ignore policy used to prune directories.
	// When nil, Walk compiles the root's .devstrapignore itself (falling back
	// to defaults with a warning on a compile error). Tests inject a Matcher
	// here to avoid touching the real filesystem's .devstrapignore.
	Ignore *ignore.Matcher
}

func Walk(ctx context.Context, root string, opts Options) (Result, error) {
	if opts.Git.Bin == "" {
		opts.Git = dsgit.NewRunner()
	}
	cleanRoot, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return Result{}, fmt.Errorf("resolve scan root: %w", err)
	}
	info, err := os.Stat(cleanRoot)
	if err != nil {
		return Result{}, fmt.Errorf("stat scan root: %w", err)
	}
	if !info.IsDir() {
		return Result{}, fmt.Errorf("scan root is not a directory: %s", cleanRoot)
	}
	result := Result{Root: cleanRoot}
	matcher := opts.Ignore
	if matcher == nil {
		m, err := ignore.CompileFromDir(cleanRoot, true)
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("ignore compile failed, using defaults: %v", err))
			m = ignore.DefaultMatcher()
		}
		matcher = m
	}
	prunedDirs := 0
	seenKeys := map[string]string{}
	remotePaths := map[string][]string{}
	var plainCandidates, claimed []string
	err = filepath.WalkDir(cleanRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("%s: %v", relOrBase(cleanRoot, path), walkErr))
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if path == cleanRoot {
			return nil
		}
		rel, err := filepath.Rel(cleanRoot, path)
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("%s: relative path failed: %v", path, err))
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		name := d.Name()
		if d.Type()&fs.ModeSymlink != 0 {
			switch err := pathkey.CheckSymlinkWithinRoot(cleanRoot, path); {
			case errors.Is(err, pathkey.ErrEscape):
				// Hard exclusion: never create a Finding for an escaping
				// symlink, and surface it as a blocking conflict on adopt.
				result.Warnings = append(result.Warnings, fmt.Sprintf("symlink escape (excluded): %s", relSlash))
			case err != nil:
				// Dangling or IO error: advisory only, not a security block.
				result.Warnings = append(result.Warnings, fmt.Sprintf("symlink unresolved: %s: %v", relSlash, err))
			}
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() && matcher.ShouldPruneDir(name, relSlash) {
			prunedDirs++
			return filepath.SkipDir
		}
		if !d.IsDir() && ignore.IsSecretPath(relSlash) {
			result.Warnings = append(result.Warnings, fmt.Sprintf("secret-looking file found: %s", relSlash))
			result.Secrets = append(result.Secrets, relSlash)
		}
		if !d.IsDir() {
			return nil
		}
		pk, err := pathkey.Clean(relSlash)
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("invalid path %s: %v", relSlash, err))
			return filepath.SkipDir
		}
		if prev, ok := seenKeys[pk.Key]; ok && prev != pk.Display {
			result.Warnings = append(result.Warnings, fmt.Sprintf("case-only path conflict: %s and %s", prev, pk.Display))
			return filepath.SkipDir
		}
		seenKeys[pk.Key] = pk.Display
		if dsgit.IsRepo(path) {
			f := Finding{Path: pk.Display, Type: TypeGitRepo}
			if remote, err := opts.Git.RemoteURL(ctx, path); err == nil {
				// Only persist a remote that passes validation. An unvalidated
				// origin (e.g. ext::/--upload-pack injection) must never be
				// stored, or scan->adopt->hydrate would later feed it to git.
				if key, err := dsgit.CanonicalRemoteKey(remote); err == nil {
					f.RemoteURL = remote
					f.RemoteKey = key
					remotePaths[key] = append(remotePaths[key], pk.Display)
				} else {
					// NOVCS-01: unvalidated remote → treat as local-only so
					// it is never adopted as a clonable git_repo that would
					// be broken on every other device.
					f.Type = TypeLocalGit
					f.Warnings = append(f.Warnings, fmt.Sprintf("ignoring unvalidated git remote: %v", err))
				}
			} else {
				// NOVCS-01: no origin → never a clonable git_repo; classify
				// as local_git so the namespace entry is not broken off-device.
				f.Type = TypeLocalGit
				f.Warnings = append(f.Warnings, "git repo has no remote; add one with 'git remote add origin <url>'")
			}
			// P6-XP-05: scan stays offline — resolve the default branch from
			// local refs only. Authoritative set-head --auto repair happens at
			// materialization (hydrate/worktree), not during the walk.
			if branch, src, err := opts.Git.LocalDefaultBranch(ctx, path, "main"); err == nil {
				f.DefaultBranch = branch
				if src == dsgit.DefaultBranchStored {
					f.Warnings = append(f.Warnings, "default branch not set locally (origin/HEAD missing); using \"main\" — will be resolved authoritatively at materialization")
				}
			} else {
				f.DefaultBranch = "main"
				f.Warnings = append(f.Warnings, "default branch unresolved offline; using \"main\" — will be resolved authoritatively at materialization")
			}
			result.Findings = append(result.Findings, f)
			return filepath.SkipDir
		}
		if opts.IncludeNonGit && HasSkeletonMarker(path) {
			// An un-materialized project waiting for content, already in the
			// namespace with a richer type. Emitting nothing keeps the walk's
			// existing behavior, but it must still be claimed: left unclaimed
			// it looks like empty ground, and a grouping ancestor would be
			// classified plain_folder and swallow the whole subtree.
			claimed = append(claimed, pk.Display)
			return filepath.SkipDir
		}
		if opts.IncludeNonGit && looksLikeProject(path) {
			result.Findings = append(result.Findings, Finding{Path: pk.Display, Type: TypeDraftFolder})
			return filepath.SkipDir
		}
		if opts.IncludeNonGit {
			plainCandidates = append(plainCandidates, pk.Display)
		}
		return nil
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		return Result{}, err
	}
	// Only a walk that ran to completion can establish that a directory groups
	// nothing. A cancelled walk still returns the findings it did make, but a
	// candidate whose subtree it never reached would look like empty ground.
	if opts.IncludeNonGit && err == nil {
		for _, f := range result.Findings {
			claimed = append(claimed, f.Path)
		}
		result.Findings = append(result.Findings, plainFolders(plainCandidates, claimed)...)
	}
	result.PrunedDirs = prunedDirs
	for key, paths := range remotePaths {
		if len(paths) < 2 {
			continue
		}
		sort.Strings(paths)
		result.Duplicates = append(result.Duplicates, Duplicate{
			RemoteKey:       key,
			Paths:           append([]string(nil), paths...),
			RecommendedPath: paths[0],
		})
	}
	sort.Slice(result.Findings, func(i, j int) bool { return result.Findings[i].Path < result.Findings[j].Path })
	sort.Slice(result.Duplicates, func(i, j int) bool { return result.Duplicates[i].RemoteKey < result.Duplicates[j].RemoteKey })
	return result, err
}

// plainFolders turns candidate directories — those that are neither a git
// repository nor a recognized project — into plain_folder findings (NOVCS-02),
// so the namespace can carry "this path exists" for grouping folders,
// documentation buckets, and local-only areas.
//
// The decision is deferred to here, after the walk, rather than taken inline
// with a SkipDir. filepath.WalkDir is pre-order: when the walk first reaches
// `work/` it cannot yet know that `work/acme/api-server` below it is a git
// repo. Classifying a bare grouping directory on sight and skipping it would
// hide every project underneath — the canonical managed tree is exactly that
// shape — so the walk records candidates and keeps recursing (which also keeps
// secret-file and symlink-escape detection alive inside them). Only now, with
// every real finding known, do the directories that group nothing become
// findings of their own, and only the topmost of a nested run: recording
// `notes/` says everything that also recording `notes/2026/` would.
//
// claimed holds every path the walk accounted for — the real findings plus
// materialization skeletons, which are emitted as nothing but are emphatically
// not empty ground.
func plainFolders(candidates, claimed []string) []Finding {
	groups := make(map[string]bool)
	for _, path := range claimed {
		for _, ancestor := range pathAncestors(path) {
			groups[ancestor] = true
		}
	}
	leaves := make(map[string]bool, len(candidates))
	for _, candidate := range candidates {
		if !groups[candidate] {
			leaves[candidate] = true
		}
	}
	plain := make([]Finding, 0, len(leaves))
	for _, candidate := range candidates {
		if !leaves[candidate] || hasAncestorIn(leaves, candidate) {
			continue
		}
		plain = append(plain, Finding{Path: candidate, Type: TypePlainFolder})
	}
	return plain
}

// pathAncestors returns every proper ancestor of a root-relative slash path:
// "a/b/c" yields "a" and "a/b". Splitting on the separator keeps the test at
// segment boundaries, so "a/bc" is never an ancestor of "a/bcd".
func pathAncestors(path string) []string {
	var ancestors []string
	for i := 0; i < len(path); i++ {
		if path[i] == '/' {
			ancestors = append(ancestors, path[:i])
		}
	}
	return ancestors
}

func hasAncestorIn(set map[string]bool, path string) bool {
	for _, ancestor := range pathAncestors(path) {
		if set[ancestor] {
			return true
		}
	}
	return false
}

// HasSkeletonMarker reports whether a directory carries DevStrap's
// materialization placeholder — the marker `hydrate` writes for a project whose
// content has not arrived yet. It is the marker test only; a caller deciding
// whether the directory is safe to overwrite must additionally check that
// nothing else lives there.
func HasSkeletonMarker(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".devstrap", "placeholder.json"))
	return err == nil
}

func looksLikeProject(path string) bool {
	for _, name := range []string{"go.mod", "package.json", "pyproject.toml", "Cargo.toml", "README.md", "README"} {
		if _, err := os.Stat(filepath.Join(path, name)); err == nil {
			return true
		}
	}
	return false
}

func relOrBase(root, path string) string {
	if rel, err := filepath.Rel(root, path); err == nil {
		return filepath.ToSlash(rel)
	}
	return filepath.Base(path)
}
