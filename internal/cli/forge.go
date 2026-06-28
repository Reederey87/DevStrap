package cli

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/Reederey87/DevStrap/internal/childenv"
	dsgit "github.com/Reederey87/DevStrap/internal/git"
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
// gracefully.
func DetectForge(remoteURL string) ForgeKind {
	host := forgeHost(remoteURL)
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
// degradation message with a compare URL for unknown forges (FORGE-01).
func createForgePR(ctx context.Context, dir, remoteURL, baseBranch, headBranch, title, body string) (string, error) {
	kind := DetectForge(remoteURL)

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
// creation on unknown forges.
func forgeCompareURL(remoteURL, baseBranch, headBranch string) string {
	key, err := dsgit.CanonicalRemoteKey(remoteURL)
	if err != nil {
		return ""
	}
	host := forgeHost(remoteURL)
	if host == "" {
		return ""
	}
	// Extract owner/repo from the remote key (host/owner/repo form).
	rest := strings.TrimPrefix(key, host+"/")
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
