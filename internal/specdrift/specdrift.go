package specdrift

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const WorkLogPath = "spec/18_WORK_LOG.md"

type Options struct {
	RepoRoot       string
	Base           string
	Head           string
	ChangedFiles   []string
	RequireWorkLog bool
}

type Spec struct {
	Path         string
	LastReviewed string
	TracksCode   []string
}

type Report struct {
	Specs        []Spec
	ChangedFiles []string
	Findings     []string
}

func (r Report) OK() bool {
	return len(r.Findings) == 0
}

func Check(ctx context.Context, opts Options) (Report, error) {
	root := opts.RepoRoot
	if root == "" {
		root = "."
	}
	specs, findings, err := LoadSpecs(root)
	if err != nil {
		return Report{}, err
	}
	changed := opts.ChangedFiles
	if changed == nil {
		changed, err = changedFiles(ctx, root, opts.Base, opts.Head)
		if err != nil {
			return Report{}, err
		}
		if opts.Head == "" || opts.Head == "HEAD" {
			workingTree, err := workingTreeChangedFiles(ctx, root)
			if err != nil {
				return Report{}, err
			}
			changed = append(changed, workingTree...)
		}
	}
	changed = normalizePaths(changed)
	changedSet := make(map[string]bool, len(changed))
	for _, file := range changed {
		changedSet[file] = true
	}

	for _, file := range changed {
		if strings.HasPrefix(file, "spec/") {
			continue
		}
		specific, broadOnly := specsTrackingTiers(specs, file)
		switch {
		case len(specific) > 0:
			if !anyChanged(specific, changedSet) {
				findings = append(findings, fmt.Sprintf("%s changed but none of the required specific specs changed: %s", file, strings.Join(specific, ", ")))
			}
		case len(broadOnly) > 0:
			if !anyChanged(broadOnly, changedSet) {
				findings = append(findings, fmt.Sprintf("%s changed but none of the mapped broad specs changed: %s", file, strings.Join(broadOnly, ", ")))
			}
		default:
			continue
		}
	}

	if opts.RequireWorkLog && requiresWorkLog(changed) && !changedSet[WorkLogPath] {
		findings = append(findings, fmt.Sprintf("code/spec/docs changed but %s was not updated", WorkLogPath))
	}
	sort.Strings(findings)
	return Report{Specs: specs, ChangedFiles: changed, Findings: findings}, nil
}

func LoadSpecs(root string) ([]Spec, []string, error) {
	matches, err := filepath.Glob(filepath.Join(root, "spec", "*.md"))
	if err != nil {
		return nil, nil, err
	}
	sort.Strings(matches)
	specs := make([]Spec, 0, len(matches))
	var findings []string
	for _, file := range matches {
		rel, err := filepath.Rel(root, file)
		if err != nil {
			return nil, nil, err
		}
		rel = filepath.ToSlash(rel)
		spec, err := parseSpecFrontmatter(rel, file)
		if err != nil {
			findings = append(findings, err.Error())
			continue
		}
		specs = append(specs, spec)
	}
	if len(matches) == 0 {
		findings = append(findings, "no spec/*.md files found")
	}
	return specs, findings, nil
}

func parseSpecFrontmatter(relPath, file string) (Spec, error) {
	//nolint:gosec // file comes from filepath.Glob(root/spec/*.md) and is not a secret-bearing user input path.
	data, err := os.ReadFile(file)
	if err != nil {
		return Spec{}, fmt.Errorf("%s: read: %w", relPath, err)
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	if !scanner.Scan() || strings.TrimSpace(scanner.Text()) != "---" {
		return Spec{}, fmt.Errorf("%s: missing YAML frontmatter", relPath)
	}
	fields := make(map[string]string)
	closed := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "---" {
			closed = true
			break
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	if err := scanner.Err(); err != nil {
		return Spec{}, fmt.Errorf("%s: scan frontmatter: %w", relPath, err)
	}
	if !closed {
		return Spec{}, fmt.Errorf("%s: missing closing YAML frontmatter delimiter", relPath)
	}
	reviewed := fields["last_reviewed"]
	if _, err := time.Parse("2006-01-02", reviewed); err != nil {
		return Spec{}, fmt.Errorf("%s: last_reviewed must be YYYY-MM-DD", relPath)
	}
	tracksRaw, ok := fields["tracks_code"]
	if !ok {
		return Spec{}, fmt.Errorf("%s: missing tracks_code", relPath)
	}
	tracks, err := parseInlineList(tracksRaw)
	if err != nil {
		return Spec{}, fmt.Errorf("%s: tracks_code: %w", relPath, err)
	}
	return Spec{Path: relPath, LastReviewed: reviewed, TracksCode: tracks}, nil
}

func parseInlineList(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "[") || !strings.HasSuffix(raw, "]") {
		return nil, fmt.Errorf("must be an inline list")
	}
	raw = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(raw, "["), "]"))
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		item := strings.Trim(strings.TrimSpace(part), `"'`)
		if item == "" {
			return nil, fmt.Errorf("empty glob")
		}
		out = append(out, filepath.ToSlash(item))
	}
	return out, nil
}

func changedFiles(ctx context.Context, root, base, head string) ([]string, error) {
	if base == "" {
		base = "origin/main"
	}
	if head == "" {
		head = "HEAD"
	}
	//nolint:gosec // command is fixed to git; base/head are refs used as git arguments, not shell-interpolated text.
	out, err := exec.CommandContext(ctx, "git", "-C", root, "diff", "--name-only", base+"..."+head).Output()
	if err != nil {
		return nil, fmt.Errorf("git diff changed files from %s...%s: %w", base, head, err)
	}
	lines := strings.Split(string(out), "\n")
	files := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

func workingTreeChangedFiles(ctx context.Context, root string) ([]string, error) {
	//nolint:gosec // command is fixed to git status and root is passed as an argument without shell interpolation.
	out, err := exec.CommandContext(ctx, "git", "-C", root, "status", "--porcelain=v1").Output()
	if err != nil {
		return nil, fmt.Errorf("git status changed files: %w", err)
	}
	lines := strings.Split(string(out), "\n")
	files := make([]string, 0, len(lines))
	for _, line := range lines {
		if len(line) < 4 {
			continue
		}
		file := strings.TrimSpace(line[3:])
		if before, after, ok := strings.Cut(file, " -> "); ok {
			_ = before
			file = after
		}
		if file != "" {
			files = append(files, file)
		}
	}
	return files, nil
}

func normalizePaths(files []string) []string {
	out := make([]string, 0, len(files))
	seen := make(map[string]bool, len(files))
	for _, file := range files {
		file = path.Clean(filepath.ToSlash(strings.TrimSpace(file)))
		if file == "." || file == "" || seen[file] {
			continue
		}
		seen[file] = true
		out = append(out, file)
	}
	sort.Strings(out)
	return out
}

func specsTrackingTiers(specs []Spec, file string) ([]string, []string) {
	var specific []string
	var broadOnly []string
	for _, spec := range specs {
		matchedBroad := false
		matchedSpecific := false
		for _, pattern := range spec.TracksCode {
			if !globMatch(pattern, file) {
				continue
			}
			if pattern == "**" {
				continue
			}
			if isBroadPattern(pattern) {
				matchedBroad = true
				continue
			}
			matchedSpecific = true
		}
		switch {
		case matchedSpecific:
			specific = append(specific, spec.Path)
		case matchedBroad:
			broadOnly = append(broadOnly, spec.Path)
		}
	}
	sort.Strings(specific)
	sort.Strings(broadOnly)
	return specific, broadOnly
}

func isBroadPattern(pattern string) bool {
	switch filepath.ToSlash(pattern) {
	case "**", "internal/**", "cmd/**":
		return true
	default:
		return false
	}
}

func globMatch(pattern, file string) bool {
	pattern = filepath.ToSlash(pattern)
	file = filepath.ToSlash(file)
	if pattern == "**" {
		return true
	}
	if strings.HasSuffix(pattern, "/**") {
		prefix := strings.TrimSuffix(pattern, "**")
		return strings.HasPrefix(file, prefix)
	}
	ok, err := path.Match(pattern, file)
	return err == nil && ok
}

func anyChanged(files []string, changedSet map[string]bool) bool {
	for _, file := range files {
		if changedSet[file] {
			return true
		}
	}
	return false
}

func requiresWorkLog(changed []string) bool {
	for _, file := range changed {
		if file == WorkLogPath {
			continue
		}
		switch {
		case strings.HasPrefix(file, "cmd/"),
			strings.HasPrefix(file, "internal/"),
			strings.HasPrefix(file, ".github/"),
			strings.HasPrefix(file, "scripts/"),
			strings.HasPrefix(file, "spec/"),
			file == ".goreleaser.yaml",
			file == "AGENTS.md",
			file == "CLAUDE.md",
			file == "README.md",
			file == "CONTRIBUTING.md",
			file == "SECURITY.md",
			file == "CODEOWNERS",
			file == "go.mod",
			file == "go.sum",
			strings.HasPrefix(file, "docs/"):
			return true
		}
	}
	return false
}
