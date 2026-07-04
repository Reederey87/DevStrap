package scan

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

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
}

type Duplicate struct {
	RemoteKey       string   `json:"remote_key"`
	Paths           []string `json:"paths"`
	RecommendedPath string   `json:"recommended_path"`
}

type Options struct {
	IncludePlainFolders bool
	Git                 dsgit.Runner
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
	seenKeys := map[string]string{}
	remotePaths := map[string][]string{}
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
		if d.IsDir() && shouldPruneDir(name, relSlash) {
			return filepath.SkipDir
		}
		if !d.IsDir() && isSecretName(name, relSlash) {
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
		if opts.IncludePlainFolders && looksLikeProject(path) {
			result.Findings = append(result.Findings, Finding{Path: pk.Display, Type: TypeDraftFolder})
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		return Result{}, err
	}
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

// pruneMatcher is the single compiled ignore policy used for directory pruning
// (DRAFT-03). The scanner prune predicate, watcher skip set, bundle allow-list,
// and agent deny-list all share one source of truth via internal/ignore.
var pruneMatcher = ignore.DefaultMatcher()

func shouldPruneDir(name, rel string) bool {
	return pruneMatcher.ShouldPruneDir(name, rel)
}

func isSecretName(name, rel string) bool {
	switch name {
	case ".env", "credentials.json", "service-account.json", "id_rsa", "id_ed25519":
		return true
	}
	if strings.HasPrefix(name, ".env.") && name != ".env.example" && name != ".env.template" && name != ".env.schema" {
		return true
	}
	if strings.HasSuffix(name, ".pem") {
		return true
	}
	return strings.HasSuffix(rel, "/.snowflake/config.toml") || strings.HasSuffix(rel, "/.aws/credentials") || strings.Contains(name, "service-account")
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
