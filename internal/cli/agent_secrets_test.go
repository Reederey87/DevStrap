package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/agentsecrets"
)

// P4-GIT-06 (project-env allowlists): `agent run` only injects
// captured/hydrated env-profile secrets when the project's fresh worktree
// carries a .devstrapagent.yml opt-in, and even then only the allowed
// (non-denied) keys reach the child process.

const (
	agentSecretsAllowedMarker = "allowed-marker-fedcba9876"
	agentSecretsDeniedMarker  = "denied-marker-1234567890"
)

// setupAgentSecretsRepo creates a bare remote seeded with an optional
// .devstrapagent.yml (agentConfig == "" omits the file entirely), registers it
// as a project, and captures a .env with one ALLOWED_VAR and one DENIED_VAR
// binding. It returns the namespace path.
func setupAgentSecretsRepo(t *testing.T, home, root, nsPath, agentConfig string) {
	t.Helper()
	tmp := t.TempDir()
	remote := filepath.Join(tmp, "repo.git")
	seed := filepath.Join(tmp, "seed")
	runGit(t, tmp, "init", "--bare", remote)
	runGit(t, seed, "init")
	runGit(t, seed, "config", "user.email", "devstrap@example.test")
	runGit(t, seed, "config", "user.name", "DevStrap Test")
	runGit(t, seed, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, seed, "add", "README.md")
	if agentConfig != "" {
		if err := os.WriteFile(filepath.Join(seed, agentsecrets.FileName), []byte(agentConfig), 0o644); err != nil {
			t.Fatal(err)
		}
		runGit(t, seed, "add", agentsecrets.FileName)
	}
	runGit(t, seed, "commit", "-m", "initial")
	runGit(t, seed, "remote", "add", "origin", remote)
	runGit(t, seed, "push", "origin", "main")
	runGit(t, tmp, "--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/main")

	if _, stderr, err := executeForTest("--home", home, "add", "file://"+remote, "--path", nsPath, "--default-branch", "main"); err != nil {
		t.Fatalf("add stderr = %q err = %v", stderr, err)
	}

	// Write the source .env file OUTSIDE the project's managed local path: that
	// path is still an empty skeleton at this point, and `agent run` clones
	// into it later -- `env capture` accepts an absolute env-file path, so
	// nothing needs to touch (and later conflict with) the skeleton directory.
	envPath := filepath.Join(tmp, "source.env")
	if err := os.WriteFile(envPath, []byte(
		"ALLOWED_VAR="+agentSecretsAllowedMarker+"\n"+
			"DENIED_VAR="+agentSecretsDeniedMarker+"\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := executeForTest("--home", home, "--root", root, "env", "capture", nsPath, envPath); err != nil {
		t.Fatalf("env capture stderr = %q err = %v", stderr, err)
	}
}

// TestAgentRunInjectsOnlyAllowlistedSecrets is the security-critical test: a
// captured var NOT on the allowlist (or explicitly denied) must never reach
// the agent subprocess environment, while an allowed var does.
func TestAgentRunInjectsOnlyAllowlistedSecrets(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	setupAgentSecretsRepo(t, home, root, "work/acme/allowlisted-repo", `
agent_secrets:
  allow:
    - ALLOWED_VAR
    - DENIED_VAR
  deny:
    - DENIED_VAR
`)

	stdout, stderr, err := executeForTest("--home", home, "--root", root,
		"agent", "run", "work/acme/allowlisted-repo",
		"--engine", "generic", "--task", "print env", "--sandbox", "off",
		"--", "printenv")
	if err != nil {
		t.Fatalf("agent run stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	if !strings.Contains(stdout, "ALLOWED_VAR="+agentSecretsAllowedMarker) {
		t.Fatalf("agent subprocess env missing the allowlisted var; stdout = %q", stdout)
	}
	if strings.Contains(stdout, agentSecretsDeniedMarker) || strings.Contains(stdout, "DENIED_VAR") {
		t.Fatalf("agent subprocess env leaked the denied var (deny must win over allow); stdout = %q", stdout)
	}
}

// TestAgentRunWithoutConfigFileInjectsNoSecrets pins the no-regression
// guarantee: a project with no .devstrapagent.yml behaves exactly as before
// this feature shipped -- no captured profile secrets reach the agent
// subprocess, allowlisted or not.
func TestAgentRunWithoutConfigFileInjectsNoSecrets(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	setupAgentSecretsRepo(t, home, root, "work/acme/no-config-repo", "")

	stdout, stderr, err := executeForTest("--home", home, "--root", root,
		"agent", "run", "work/acme/no-config-repo",
		"--engine", "generic", "--task", "print env", "--sandbox", "off",
		"--", "printenv")
	if err != nil {
		t.Fatalf("agent run stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	if strings.Contains(stdout, agentSecretsAllowedMarker) || strings.Contains(stdout, agentSecretsDeniedMarker) {
		t.Fatalf("agent subprocess env leaked a captured secret with no .devstrapagent.yml present; stdout = %q", stdout)
	}
	if strings.Contains(stdout, "ALLOWED_VAR") || strings.Contains(stdout, "DENIED_VAR") {
		t.Fatalf("agent subprocess env exposed a captured var name with no .devstrapagent.yml present; stdout = %q", stdout)
	}
}

// TestAgentRunConfigPresentButEmptyDeniesEverything pins the "presence is an
// explicit opt-in" rule: a .devstrapagent.yml with no agent_secrets.allow
// entries exposes nothing, even though a captured profile exists.
func TestAgentRunConfigPresentButEmptyDeniesEverything(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	setupAgentSecretsRepo(t, home, root, "work/acme/empty-config-repo", "# no agent_secrets key\n")

	stdout, stderr, err := executeForTest("--home", home, "--root", root,
		"agent", "run", "work/acme/empty-config-repo",
		"--engine", "generic", "--task", "print env", "--sandbox", "off",
		"--", "printenv")
	if err != nil {
		t.Fatalf("agent run stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	if strings.Contains(stdout, agentSecretsAllowedMarker) || strings.Contains(stdout, agentSecretsDeniedMarker) {
		t.Fatalf("agent subprocess env leaked a captured secret with an empty agent_secrets policy; stdout = %q", stdout)
	}
}
