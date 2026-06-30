package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Reederey87/DevStrap/internal/childenv"
	dsgit "github.com/Reederey87/DevStrap/internal/git"
	"github.com/spf13/viper"
)

// ForgeKind identifies the forge hosting a repository (FORGE-01).
type ForgeKind string

const (
	ForgeGitHub    ForgeKind = "github"
	ForgeGitLab    ForgeKind = "gitlab"
	ForgeGitea     ForgeKind = "gitea"
	ForgeBitbucket ForgeKind = "bitbucket"
	ForgeAzure     ForgeKind = "azure"
	ForgeUnknown   ForgeKind = ""
)

// DetectForge infers the forge kind from a remote URL's host (FORGE-01).
// For self-hosted instances the host will not match known patterns and
// ForgeUnknown is returned; the caller should prompt for --forge or degrade
// gracefully. SSH host aliases (~/.ssh/config) are resolved first so
// `git@work-gitlab:org/repo` maps to the real host (GIT-05).
func DetectForge(remoteURL string) ForgeKind {
	host := resolveForgeHost(remoteURL)
	switch {
	case strings.Contains(host, "github."):
		return ForgeGitHub
	case strings.Contains(host, "gitlab."):
		return ForgeGitLab
	case strings.Contains(host, "bitbucket.org"):
		return ForgeBitbucket
	case strings.Contains(host, "dev.azure.com"), strings.Contains(host, "visualstudio.com"):
		return ForgeAzure
	case strings.Contains(host, "gitea."), strings.Contains(host, "codeberg.org"),
		strings.Contains(host, "forgejo"):
		return ForgeGitea
	default:
		return ForgeUnknown
	}
}

// parseForgeKind validates a forge-kind string from a flag, config, or DB
// column and returns the typed ForgeKind (GIT-05). An empty string means
// "not set" and returns ForgeUnknown with ok=false so callers can fall through
// to the next precedence tier.
func parseForgeKind(s string) (ForgeKind, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ForgeUnknown, false
	}
	switch ForgeKind(s) {
	case ForgeGitHub, ForgeGitLab, ForgeGitea, ForgeBitbucket, ForgeAzure:
		return ForgeKind(s), true
	default:
		return ForgeUnknown, false
	}
}

// ResolveForge resolves the forge kind for a remote with a documented
// precedence (GIT-05): --forge flag > per-project git_repos.forge_kind column
// > [forge] host map (config) > DetectForge heuristic. Each tier is only
// consulted if the prior tier is unset/invalid, so a self-hosted GitLab at
// git.acme.com can be taught once (host map or project column) and then route
// to glab without per-call flags.
func ResolveForge(remoteURL, flagForge, projectForge string, hostMap map[string]ForgeKind) ForgeKind {
	if k, ok := parseForgeKind(flagForge); ok {
		return k
	}
	if k, ok := parseForgeKind(projectForge); ok {
		return k
	}
	if hostMap != nil {
		host := resolveForgeHost(remoteURL)
		if k, ok := hostMap[host]; ok {
			return k
		}
	}
	return DetectForge(remoteURL)
}

// forgeHostMap reads the `[forge]` host->kind map from config (GIT-05), e.g.:
//
//	forge:
//	  git.acme.com: gitlab
//	  scm.internal: gitea
func forgeHostMap(v *viper.Viper) map[string]ForgeKind {
	if v == nil {
		return nil
	}
	raw := v.GetStringMapString("forge")
	if len(raw) == 0 {
		return nil
	}
	m := make(map[string]ForgeKind, len(raw))
	for host, kind := range raw {
		if k, ok := parseForgeKind(kind); ok {
			m[strings.ToLower(strings.TrimSpace(host))] = k
		}
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// resolveForgeHost extracts the hostname from a git remote URL and resolves any
// SSH host alias (~/.ssh/config Host -> HostName) so a configured alias like
// `work-gitlab` maps to the real self-hosted host before forge detection
// (GIT-05). Parsing failures fall back to the literal extracted host.
func resolveForgeHost(remoteURL string) string {
	host := forgeHost(remoteURL)
	if host == "" {
		return ""
	}
	if real := resolveSSHHostAlias(host); real != "" {
		return strings.ToLower(real)
	}
	return host
}

// resolveSSHHostAlias resolves a host alias to its real HostName (GIT-05). It
// prefers `ssh -G <alias>`, which is authoritative — it honors Include, Match,
// negation, tokens, and `key=value` syntax that the hand-rolled parser ignores
// (P5-CLI-04) — and falls back to parsing ~/.ssh/config directly when ssh is
// unavailable. Returns "" when there is no real HostName override (forge
// detection then treats the literal alias as the host).
func resolveSSHHostAlias(alias string) string {
	alias = strings.ToLower(strings.TrimSpace(alias))
	// Reject empty, whitespace/path-bearing, and leading-dash aliases. The
	// leading-dash check (P5 review) prevents an attacker-influenced remote host
	// (a host string from a synced namespace event) from being parsed as an
	// `ssh -G` option (e.g. `-Fsomefile`); ssh does not honor `--` as an
	// end-of-options guard, so rejection is the robust fix.
	if alias == "" || strings.ContainsAny(alias, " /\t") || strings.HasPrefix(alias, "-") {
		return ""
	}
	// P5-CLI-04: prefer `ssh -G`, which is authoritative when it finds a real
	// override (it honors Include/Match/key=value/tokens the file parser ignores).
	// `ssh -G` echoes the alias as `hostname` when there is no override, so a
	// result equal to the alias means "no override found" — fall back to the file
	// parser (which honors negation, P5 review) rather than guessing.
	if host, ok := sshDashGHostName(alias); ok && host != alias {
		return host
	}
	return resolveSSHHostAliasFromFile(alias)
}

// sshDashGHostName runs `ssh -G <alias>` and returns (resolvedHostname, ok).
// ok is true only when ssh ran successfully; the hostname is lowercased and
// equals the alias when there is no HostName override. ok is false when ssh is
// unavailable or errors, signalling the caller to fall back to the file parser
// (P5-CLI-04). It is bounded by a short timeout and a sanitized environment;
// `ssh -G` only resolves config and does not connect.
func sshDashGHostName(alias string) (string, bool) {
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	//nolint:gosec // alias is validated (no spaces/slashes/leading-dash) and ssh -G only resolves config.
	cmd := exec.CommandContext(ctx, sshPath, "-G", alias)
	if env, eerr := childenv.FromOS(childenv.BasicAllowlist(), nil); eerr == nil {
		cmd.Env = env
	}
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.ToLower(fields[0]) == "hostname" {
			return strings.ToLower(fields[1]), true
		}
	}
	// ssh ran but printed no hostname line (unexpected); treat as no override.
	return alias, true
}

// resolveSSHHostAliasFromFile looks up a host alias in ~/.ssh/config and returns
// the matching HostName, or "" if not found / unreadable. Matching supports
// single-segment aliases and `*`/`?` glob patterns as ssh does. Only the first
// matching Host block is honored (ssh semantics). Used as the fallback when
// `ssh -G` is unavailable (P5-CLI-04).
func resolveSSHHostAliasFromFile(alias string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	//nolint:gosec // Intentional read of the user's own ~/.ssh/config to resolve forge host aliases (GIT-05); home is from os.UserHomeDir and the path is a fixed, well-known location.
	f, err := os.Open(filepath.Clean(filepath.Join(home, ".ssh", "config")))
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	inMatch := false
	var hostName string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		key := strings.ToLower(fields[0])
		switch key {
		case "host":
			// A new Host block starts. If the previous block matched and had a
			// HostName, return it; otherwise begin matching the new patterns.
			if inMatch && hostName != "" {
				return hostName
			}
			inMatch = false
			hostName = ""
			// P5 review: honor OpenSSH negation. A `!pattern` that matches the
			// alias vetoes the whole Host block even if a positive pattern also
			// matches, so the file parser does not return a host ssh would
			// exclude.
			matched, negated := false, false
			for _, pat := range fields[1:] {
				if strings.HasPrefix(pat, "!") {
					if sshHostMatch(pat[1:], alias) {
						negated = true
					}
				} else if sshHostMatch(pat, alias) {
					matched = true
				}
			}
			if matched && !negated {
				inMatch = true
			}
		case "hostname":
			if inMatch && hostName == "" {
				hostName = fields[1]
			}
		default:
			// Ignore other directives (User, Port, IdentityFile, ...).
		}
	}
	if inMatch && hostName != "" {
		return hostName
	}
	return ""
}

// sshHostMatch reports whether an ssh-config Host pattern matches a host alias,
// supporting `*` and `?` glob segments the way OpenSSH does.
func sshHostMatch(pattern, host string) bool {
	pattern = strings.ToLower(pattern)
	if pattern == "*" {
		return true
	}
	ok, err := filepath.Match(pattern, host)
	return err == nil && ok
}

// forgeHost extracts the hostname from a git remote URL (ssh, https, or
// scp-like). It reuses the git package's URL parsing.
func forgeHost(remoteURL string) string {
	// Try url.Parse for https:// and ssh:// forms.
	if host, ok := hostFromURL(remoteURL); ok {
		return host
	}
	// Fall back to scp-like: git@host:owner/repo
	if idx := strings.Index(remoteURL, ":"); idx > 0 {
		if at := strings.Index(remoteURL[:idx], "@"); at >= 0 {
			return strings.ToLower(remoteURL[at+1 : idx])
		}
		return strings.ToLower(remoteURL[:idx])
	}
	return ""
}

func hostFromURL(remoteURL string) (string, bool) {
	for _, scheme := range []string{"https://", "http://", "ssh://", "git://"} {
		if strings.HasPrefix(remoteURL, scheme) {
			rest := remoteURL[len(scheme):]
			if at := strings.Index(rest, "@"); at >= 0 {
				rest = rest[at+1:]
			}
			host := rest
			if slash := strings.Index(host, "/"); slash > 0 {
				host = host[:slash]
			}
			if colon := strings.Index(host, ":"); colon > 0 {
				host = host[:colon]
			}
			return strings.ToLower(host), true
		}
	}
	return "", false
}

// forgeTokenEnv returns the environment variable patterns that should be
// allowed for the given forge's PR creation command (FORGE-02).
func forgeTokenEnv(k ForgeKind) []string {
	switch k {
	case ForgeGitHub:
		return []string{"GH_*", "GITHUB_TOKEN"}
	case ForgeGitLab:
		return []string{"GITLAB_TOKEN", "GLAB_*", "CI_JOB_TOKEN"}
	case ForgeGitea:
		return []string{"GITEA_TOKEN", "FORGEJO_TOKEN", "TEA_*"}
	case ForgeBitbucket:
		return []string{"BITBUCKET_TOKEN", "BITBUCKET_USERNAME", "BITBUCKET_APP_PASSWORD"}
	case ForgeAzure:
		return []string{"AZURE_DEVOPS_EXT_PAT", "SYSTEM_ACCESSTOKEN"}
	default:
		return nil
	}
}

// forgeCLI returns the CLI binary name for the given forge.
func forgeCLI(k ForgeKind) string {
	switch k {
	case ForgeGitHub:
		return "gh"
	case ForgeGitLab:
		return "glab"
	case ForgeGitea:
		return "tea"
	default:
		return ""
	}
}

// forgePRCommand builds the argv for creating a PR/MR on the given forge.
func forgePRCommand(k ForgeKind, baseBranch, headBranch, title, body string) []string {
	switch k {
	case ForgeGitHub:
		return []string{"gh", "pr", "create", "--base", baseBranch, "--head", headBranch, "--title", title, "--body", body}
	case ForgeGitLab:
		return []string{"glab", "mr", "create", "--target-branch", baseBranch, "--source-branch", headBranch, "--title", title, "--description", body}
	case ForgeGitea:
		return []string{"tea", "pr", "create", "--base", baseBranch, "--head", headBranch, "--title", title, "--description", body}
	default:
		return nil
	}
}

// createForgePR creates a PR/MR on the detected forge, or prints a graceful
// degradation message with a compare URL for unknown forges (FORGE-01). Forge
// resolution honors the GIT-05 precedence: --forge flag > project column >
// [forge] host map > DetectForge heuristic.
func createForgePR(ctx context.Context, dir, remoteURL, baseBranch, headBranch, title, body, forgeOverride, projectForge string, hostMap map[string]ForgeKind) (string, error) {
	kind := ResolveForge(remoteURL, forgeOverride, projectForge, hostMap)

	if kind == ForgeUnknown || forgeCLI(kind) == "" {
		// Graceful degradation: the branch is already pushed; print a
		// compare/MR URL so the user can create the PR manually (FORGE-01).
		compareURL := forgeCompareURL(remoteURL, baseBranch, headBranch)
		msg := fmt.Sprintf("branch %s pushed; forge not auto-detected from %s", headBranch, remoteURL)
		if compareURL != "" {
			msg += "\nOpen a merge request manually: " + compareURL
		}
		return msg, nil
	}

	// Preflight: check the CLI binary exists.
	cli := forgeCLI(kind)
	if _, err := exec.LookPath(cli); err != nil {
		compareURL := forgeCompareURL(remoteURL, baseBranch, headBranch)
		msg := fmt.Sprintf("branch %s pushed; %s CLI not found in PATH", headBranch, cli)
		if compareURL != "" {
			msg += "\nOpen a merge request manually: " + compareURL
		}
		return msg, nil
	}

	args := forgePRCommand(kind, baseBranch, headBranch, title, body)
	env, err := childenv.FromOS(append(childenv.BasicAllowlist(), forgeTokenEnv(kind)...), nil)
	if err != nil {
		return "", err
	}
	command := exec.CommandContext(ctx, args[0], args[1:]...) //nolint:gosec // fixed forge CLI command with explicit argv and sanitized env.
	command.Dir = dir
	command.Env = env
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", appError{code: exitGit, err: fmt.Errorf("%s pr create failed: %s", cli, msg)}
	}
	return strings.TrimSpace(stdout.String()), nil
}

// forgeCompareURL constructs a best-effort compare/MR URL for manual PR
// creation on unknown forges. The SSH-alias-resolved host (GIT-05) is used for
// the URL so a configured alias like `work-gitlab` maps to the real,
// browser-usable host; the raw host is still used to trim the canonical key
// (CanonicalRemoteKey does not resolve aliases).
func forgeCompareURL(remoteURL, baseBranch, headBranch string) string {
	key, err := dsgit.CanonicalRemoteKey(remoteURL)
	if err != nil {
		return ""
	}
	rawHost := forgeHost(remoteURL)
	if rawHost == "" {
		return ""
	}
	host := resolveForgeHost(remoteURL)
	if host == "" {
		host = rawHost
	}
	// Extract owner/repo from the remote key (rawHost/owner/repo form).
	rest := strings.TrimPrefix(key, rawHost+"/")
	scheme := "https"
	switch {
	case strings.Contains(host, "github."):
		return fmt.Sprintf("%s://%s/%s/compare/%s...%s", scheme, host, rest, baseBranch, headBranch)
	case strings.Contains(host, "gitlab."):
		return fmt.Sprintf("%s://%s/%s/-/merge_requests/new?merge_request[source_branch]=%s&merge_request[target_branch]=%s", scheme, host, rest, headBranch, baseBranch)
	default:
		return fmt.Sprintf("%s://%s/%s/compare/%s...%s", scheme, host, rest, baseBranch, headBranch)
	}
}
