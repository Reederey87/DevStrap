// Package ignore is the canonical .devstrapignore compiler (DRAFT-03). It is
// the single source of truth for what content is excluded from draft bundles,
// pruned by the scanner, skipped by the watcher, denied to agents, and emitted
// into generated .gitignore/.dockerignore fragments. One compiled policy feeds
// every consumer so the four previously-divergent hardcoded lists cannot drift.
//
// Pattern semantics follow .gitignore (https://git-scm.com/docs/gitignore):
//   - Blank lines and lines starting with "#" are ignored.
//   - A leading "!" negates a pattern (last matching pattern wins).
//   - A trailing "/" makes the pattern directory-only.
//   - A leading "/" anchors the pattern to the root.
//   - A leading "**/" matches in all directories.
//   - A trailing "/**" matches everything inside.
//   - "*" matches anything except "/"; "?" matches any single char except "/".
//   - "**" between slashes matches zero or more directories.
package ignore

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Pattern is a single compiled .devstrapignore pattern.
type Pattern struct {
	negate   bool
	dirOnly  bool
	anchored bool
	regex    *regexp.Regexp
	text     string
}

// Matcher is an ordered list of compiled patterns evaluated last-match-wins,
// exactly like .gitignore. A Matcher is safe for concurrent Match calls once
// construction is complete.
type Matcher struct {
	patterns []Pattern
}

// Match reports whether relPath (forward-slash relative to the project root) is
// ignored. isDir indicates whether the path is a directory so that
// directory-only (trailing-slash) patterns match correctly.
func (m *Matcher) Match(relPath string, isDir bool) bool {
	if m == nil || len(m.patterns) == 0 {
		return false
	}
	relPath = filepath.ToSlash(relPath)
	ignored := false
	for _, p := range m.patterns {
		if !p.matches(relPath, isDir) {
			continue
		}
		if p.negate {
			ignored = false
		} else {
			ignored = true
		}
	}
	return ignored
}

// ShouldPruneDir is a fast path for the scanner walk: it reports whether a
// directory entry should be pruned (skipped entirely) based on the compiled
// policy. name is the directory base name; relSlash is the forward-slash path
// relative to the scan root.
func (m *Matcher) ShouldPruneDir(name, relSlash string) bool {
	if m == nil {
		return DefaultMatcher().ShouldPruneDir(name, relSlash)
	}
	if m.Match(relSlash, true) {
		return true
	}
	// Also match the bare name so unanchored patterns like "node_modules/"
	// prune even before the full relative path is known to the caller.
	return m.Match(name, true)
}

// GitignoreFragment returns the compiled patterns as a .gitignore-compatible
// text block suitable for emitting into a project's .gitignore or .dockerignore
// (DRAFT-03 generated-ignore target).
func (m *Matcher) GitignoreFragment() string {
	if m == nil {
		return DefaultGitignoreFragment()
	}
	var b strings.Builder
	for _, p := range m.patterns {
		if p.negate {
			b.WriteByte('!')
		}
		b.WriteString(p.text)
		b.WriteByte('\n')
	}
	return b.String()
}

// Compile parses source text into a Matcher. When includeDefaults is true the
// canonical OS-junk and build-artifact patterns (DefaultJunk) are prepended so
// every consumer gets the same baseline exclusions.
func Compile(source string, includeDefaults bool) (*Matcher, error) {
	var patterns []Pattern
	if includeDefaults {
		patterns = append(patterns, defaultPatterns...)
	}
	parsed, err := parseLines(strings.NewReader(source))
	if err != nil {
		return nil, err
	}
	patterns = append(patterns, parsed...)
	return &Matcher{patterns: patterns}, nil
}

// CompileFromDir reads a .devstrapignore file from dir (if present) and compiles
// it with the default junk patterns prepended.
func CompileFromDir(dir string, includeDefaults bool) (*Matcher, error) {
	path := filepath.Join(dir, ".devstrapignore")
	raw, err := os.ReadFile(path) //nolint:gosec // Path is the project's own .devstrapignore.
	if err != nil {
		if os.IsNotExist(err) {
			return Compile("", includeDefaults)
		}
		return nil, fmt.Errorf("read .devstrapignore: %w", err)
	}
	return Compile(string(raw), includeDefaults)
}

// DefaultMatcher returns a Matcher containing only the canonical OS-junk and
// build-artifact patterns.
func DefaultMatcher() *Matcher {
	return &Matcher{patterns: append([]Pattern(nil), defaultPatterns...)}
}

// DefaultGitignoreFragment returns the default junk patterns as a .gitignore
// fragment.
func DefaultGitignoreFragment() string {
	var b strings.Builder
	for _, p := range defaultPatterns {
		if p.negate {
			b.WriteByte('!')
		}
		b.WriteString(p.text)
		b.WriteByte('\n')
	}
	return b.String()
}

func parseLines(r *strings.Reader) ([]Pattern, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var patterns []Pattern
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		p, ok, err := parseLine(line)
		if err != nil {
			return nil, err
		}
		if ok {
			patterns = append(patterns, p)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan ignore patterns: %w", err)
	}
	return patterns, nil
}

func parseLine(line string) (Pattern, bool, error) {
	// Strip trailing whitespace (gitignore ignores it) but not leading.
	line = strings.TrimRight(line, " \t")
	if line == "" || strings.HasPrefix(line, "#") {
		return Pattern{}, false, nil
	}
	p := Pattern{text: line}
	if strings.HasPrefix(line, "!") {
		p.negate = true
		line = line[1:]
		p.text = line
	}
	if len(line) > 0 && line[len(line)-1] == '/' {
		p.dirOnly = true
		line = line[:len(line)-1]
	}
	if strings.HasPrefix(line, "/") {
		p.anchored = true
		line = line[1:]
	}
	if line == "" {
		return Pattern{}, false, fmt.Errorf("empty pattern after stripping prefix/suffix")
	}
	re, err := patternToRegex(line, p.anchored)
	if err != nil {
		return Pattern{}, false, fmt.Errorf("compile pattern %q: %w", p.text, err)
	}
	p.regex = re
	return p, true, nil
}

func (p Pattern) matches(path string, isDir bool) bool {
	if p.dirOnly && !isDir {
		// A directory-only pattern still matches descendants of a matched
		// directory, so test every prefix of the path.
		segs := strings.Split(path, "/")
		for end := 1; end < len(segs); end++ {
			if p.regex.MatchString(strings.Join(segs[:end], "/")) {
				return true
			}
		}
		return false
	}
	return p.regex.MatchString(path)
}

// patternToRegex converts a gitignore pattern body (after stripping !, trailing
// /, and leading /) into a regexp that matches paths relative to the root.
func patternToRegex(body string, anchored bool) (*regexp.Regexp, error) {
	var sb strings.Builder
	if anchored {
		sb.WriteString("^")
	} else {
		// Unanchored patterns match at any depth.
		sb.WriteString("(?:^|/)")
	}
	i := 0
	for i < len(body) {
		c := body[i]
		switch c {
		case '*':
			if i+1 < len(body) && body[i+1] == '*' {
				// ** — consume the double star and any surrounding slashes.
				i += 2
				if i < len(body) && body[i] == '/' {
					i++
					sb.WriteString("(?:.*/)?")
				} else {
					sb.WriteString(".*")
				}
			} else {
				sb.WriteString("[^/]*")
				i++
			}
		case '?':
			sb.WriteString("[^/]")
			i++
		case '.', '+', '(', ')', '^', '$', '|', '{', '}', '\\':
			sb.WriteByte('\\')
			sb.WriteByte(c)
			i++
		default:
			sb.WriteByte(c)
			i++
		}
	}
	sb.WriteString("/?$")
	return regexp.Compile(sb.String())
}

// defaultPatterns is the canonical OS-junk and build-artifact table (DRAFT-03).
// Every consumer (scanner prune, watcher skip, bundle walker, agent deny,
// generated gitignore) reads from this single table.
var defaultPatterns = func() []Pattern {
	lines := []string{
		// VCS metadata — never synced or bundled.
		".git/",
		// OS junk.
		".DS_Store",
		"Thumbs.db",
		"ehthumbs.db",
		".AppleDouble",
		".LSOverride",
		"desktop.ini",
		// Language/runtime build artifacts — rebuilt on hydrate, never synced (DRAFT-05).
		"node_modules/",
		"dist/",
		"build/",
		"out/",
		"target/",
		"bin/",
		"obj/",
		".next/",
		".nuxt/",
		".turbo/",
		".gradle/",
		".stack-work/",
		"_build/",
		"__pycache__/",
		".pytest_cache/",
		".mypy_cache/",
		".ruff_cache/",
		".ipynb_checkpoints/",
		// Virtual environments — platform-specific, rebuilt locally.
		".venv/",
		"venv/",
		"env/",
		// Coverage / test artifacts.
		"coverage/",
		".nyc_output/",
		"checkpoints/",
		// ML data-pipeline conventions (excluded from sync, not artifacts).
		"data/raw/",
		"data/interim/",
		// DevStrap internal dirs.
		".devstrap/tmp/",
		".devstrap/cache/",
	}
	patterns := make([]Pattern, 0, len(lines))
	for _, line := range lines {
		p, ok, err := parseLine(line)
		if err != nil || !ok {
			panic(fmt.Sprintf("invalid default ignore pattern %q: %v", line, err))
		}
		patterns = append(patterns, p)
	}
	return patterns
}()
