package agentsecrets

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadFromDirMissingFileReturnsNilPolicy(t *testing.T) {
	policy, err := LoadFromDir(t.TempDir())
	if err != nil {
		t.Fatalf("LoadFromDir: %v", err)
	}
	if policy != nil {
		t.Fatalf("policy = %+v, want nil for a directory with no %s", policy, FileName)
	}
}

func TestLoadFromDirParsesAllowAndDeny(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `
agent_secrets:
  allow:
    - GITHUB_TOKEN_READONLY
    - API_BASE_URL
  deny:
    - OPENAI_ADMIN_KEY
    - AWS_SECRET_ACCESS_KEY
`)
	policy, err := LoadFromDir(dir)
	if err != nil {
		t.Fatalf("LoadFromDir: %v", err)
	}
	if policy == nil {
		t.Fatal("policy = nil, want a loaded Policy")
	}
	wantAllow := []string{"GITHUB_TOKEN_READONLY", "API_BASE_URL"}
	if !equalSlices(policy.Allow, wantAllow) {
		t.Fatalf("Allow = %v, want %v", policy.Allow, wantAllow)
	}
	wantDeny := []string{"OPENAI_ADMIN_KEY", "AWS_SECRET_ACCESS_KEY"}
	if !equalSlices(policy.Deny, wantDeny) {
		t.Fatalf("Deny = %v, want %v", policy.Deny, wantDeny)
	}
}

func TestLoadFromDirMalformedYAMLErrors(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, "agent_secrets: [this is not a mapping\n")
	if _, err := LoadFromDir(dir); err == nil {
		t.Fatal("LoadFromDir succeeded on malformed YAML, want an error")
	}
}

func TestLoadFromDirPresentButEmptyDeniesEverything(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, "# no agent_secrets key at all\n")
	policy, err := LoadFromDir(dir)
	if err != nil {
		t.Fatalf("LoadFromDir: %v", err)
	}
	if policy == nil {
		t.Fatal("policy = nil, want a non-nil (deny-everything) Policy for a present-but-empty file")
	}
	got := policy.Filter(map[string]string{"ANYTHING": "value"})
	if len(got) != 0 {
		t.Fatalf("Filter = %v, want empty (no allow entries)", got)
	}
}

func TestPolicyFilterDenyWinsOnConflict(t *testing.T) {
	policy := &Policy{
		Allow: []string{"SHARED", "ALLOWED_ONLY"},
		Deny:  []string{"SHARED", "DENIED_ONLY"},
	}
	vars := map[string]string{
		"SHARED":       "shared-value",
		"ALLOWED_ONLY": "allowed-value",
		"DENIED_ONLY":  "denied-value",
		"UNLISTED":     "unlisted-value",
	}
	got := policy.Filter(vars)
	want := map[string]string{"ALLOWED_ONLY": "allowed-value"}
	if len(got) != len(want) || got["ALLOWED_ONLY"] != want["ALLOWED_ONLY"] {
		t.Fatalf("Filter = %v, want %v (SHARED denied despite being in Allow, DENIED_ONLY and UNLISTED excluded)", got, want)
	}
}

func TestPolicyFilterNilPolicyAllowsNothing(t *testing.T) {
	var policy *Policy
	got := policy.Filter(map[string]string{"ANYTHING": "value"})
	if len(got) != 0 {
		t.Fatalf("Filter on nil policy = %v, want empty", got)
	}
}

func TestPolicyFilterSkipsAllowedNameNotInVars(t *testing.T) {
	policy := &Policy{Allow: []string{"NEVER_CAPTURED"}}
	got := policy.Filter(map[string]string{"OTHER": "value"})
	if len(got) != 0 {
		t.Fatalf("Filter = %v, want empty when the allowed name was never captured", got)
	}
}

func writeConfig(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func equalSlices(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i, v := range want {
		if got[i] != v {
			return false
		}
	}
	return true
}
