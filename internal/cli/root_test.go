package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/envbundle"
	dsgit "github.com/Reederey87/DevStrap/internal/git"
	"github.com/Reederey87/DevStrap/internal/platform"
	"github.com/Reederey87/DevStrap/internal/state"
	"github.com/spf13/viper"
	"github.com/zalando/go-keyring"
)

func TestMain(m *testing.M) {
	keyring.MockInit()
	os.Exit(m.Run())
}

func executeForTest(args ...string) (string, string, error) {
	var stdout, stderr bytes.Buffer
	cmd := NewRootCommand(&stdout, &stderr)
	cmd.SetArgs(args)
	err := cmd.Execute()
	if err != nil {
		ExitCodeWithWriter(err, &stderr)
	}
	return stdout.String(), stderr.String(), err
}

func TestMissingConfigPrintsError(t *testing.T) {
	_, stderr, err := executeForTest("--config", filepath.Join(t.TempDir(), "missing.yaml"), "status")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(stderr, "read config:") {
		t.Fatalf("stderr = %q, want config error", stderr)
	}
}

func TestExitCodeMapsTypedGitErrors(t *testing.T) {
	var stderr bytes.Buffer
	if got := ExitCodeWithWriter(dsgit.ErrNetwork, &stderr); got != exitNetwork {
		t.Fatalf("ExitCodeWithWriter(ErrNetwork) = %d, want %d", got, exitNetwork)
	}
	stderr.Reset()
	if got := ExitCodeWithWriter(dsgit.ErrTimeout, &stderr); got != exitNetwork {
		t.Fatalf("ExitCodeWithWriter(ErrTimeout) = %d, want %d", got, exitNetwork)
	}
	stderr.Reset()
	if got := ExitCodeWithWriter(dsgit.ErrAuth, &stderr); got != exitAuth {
		t.Fatalf("ExitCodeWithWriter(ErrAuth) = %d, want %d", got, exitAuth)
	}
	stderr.Reset()
	if got := ExitCodeWithWriter(dsgit.ErrBranchNotFound, &stderr); got != exitGit {
		t.Fatalf("ExitCodeWithWriter(ErrBranchNotFound) = %d, want %d", got, exitGit)
	}
}

func TestGitRunnerUsesConfiguredCloneTimeout(t *testing.T) {
	v := viper.New()
	v.Set("materialization.clone_timeout", "5m")
	r := gitRunner(&options{v: v})
	if r.LongTimeout != 5*time.Minute {
		t.Fatalf("LongTimeout = %s, want 5m", r.LongTimeout)
	}
}

func TestGitRunnerDefaultCloneTimeout(t *testing.T) {
	r := gitRunner(&options{v: viper.New()})
	if r.LongTimeout != 30*time.Minute {
		t.Fatalf("LongTimeout = %s, want 30m", r.LongTimeout)
	}
}

func TestRelocatedHomeConfigIsLoaded(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "ConfiguredRoot")
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte("root: "+root+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, err := executeForTest("--home", home, "init", "--dry-run")
	if err != nil {
		t.Fatalf("stderr = %q, err = %v", stderr, err)
	}
	if !strings.Contains(stdout, root) {
		t.Fatalf("stdout = %q, want configured root %q", stdout, root)
	}
}

func TestInitDryRunIncludesLogDir(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	stdout, stderr, err := executeForTest("--home", home, "--root", root, "init", "--dry-run")
	if err != nil {
		t.Fatalf("stderr = %q, err = %v", stderr, err)
	}
	if !strings.Contains(stdout, filepath.Join(home, "logs")) {
		t.Fatalf("stdout = %q, want log dir", stdout)
	}
}

func TestInitRejectsPositionalRootWithRootFlag(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	_, stderr, err := executeForTest("--home", home, "--root", filepath.Join(t.TempDir(), "flag-root"), "init", "pos-root")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(stderr, "use either positional root or --root") {
		t.Fatalf("stderr = %q, want root precedence error", stderr)
	}
}

func TestStatusBeforeInitIsFriendly(t *testing.T) {
	home := t.TempDir()
	_, stderr, err := executeForTest("--home", home, "status")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(stderr, "workspace is not initialized; run devstrap init") {
		t.Fatalf("stderr = %q, want init hint", stderr)
	}
}

func TestInitStatusAndDBCommands(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	stdout, stderr, err := executeForTest("--home", home, "--root", root, "init", "--workspace-name", "personal")
	if err != nil {
		t.Fatalf("stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	if !strings.Contains(stdout, "Initialized DevStrap workspace") {
		t.Fatalf("stdout = %q, want init confirmation", stdout)
	}

	stdout, stderr, err = executeForTest("--home", home, "status", "--json")
	if err != nil {
		t.Fatalf("stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	var summary state.Summary
	if err := json.Unmarshal([]byte(stdout), &summary); err != nil {
		t.Fatalf("status --json is not valid JSON: %v\n%s", err, stdout)
	}
	if summary.WorkspaceName != "personal" || summary.ProjectCount != 0 {
		t.Fatalf("status summary = %+v, want personal workspace with 0 projects", summary)
	}
	if summary.DeviceID == "" {
		t.Fatalf("status summary missing device_id: %+v", summary)
	}

	stdout, stderr, err = executeForTest("--home", home, "db", "status")
	if err != nil {
		t.Fatalf("stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	if !strings.Contains(stdout, "schema version: 15") || !strings.Contains(stdout, "sqlite quick_check: ok") || !strings.Contains(stdout, "sqlite foreign_key_check: ok") {
		t.Fatalf("stdout = %q, want db status", stdout)
	}
	syncHubPath := filepath.Join(t.TempDir(), "hub.json")
	stdout, stderr, err = executeForTest("--home", home, "sync", "--hub-file", syncHubPath, "--dry-run")
	if err != nil {
		t.Fatalf("sync dry-run stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	if !strings.Contains(stdout, "Would push") {
		t.Fatalf("sync dry-run stdout = %q, want dry-run summary", stdout)
	}
	// P6-CLI-05: the dry-run line names the resolved hub ID (file:<path>), not
	// an empty target — proving the hubFile→hubID fix.
	if !strings.Contains(stdout, "file:"+syncHubPath) {
		t.Fatalf("sync dry-run stdout = %q, want resolved hub ID file:%s", stdout, syncHubPath)
	}

	store, err := state.Open(context.Background(), filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(store)
	device, err := store.CurrentDevice(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(device.PublicKey, "age1") {
		t.Fatalf("device public key = %q, want age recipient", device.PublicKey)
	}
	keyStore := devicekeys.NewHybridStore(filepath.Join(home, "keys"), platform.Detect().Keychain)
	identity, err := keyStore.Read(t.Context(), device.ID)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Recipient != device.PublicKey {
		t.Fatalf("identity recipient = %q, want %q", identity.Recipient, device.PublicKey)
	}
	signingIdentity, err := keyStore.ReadSigning(t.Context(), device.ID)
	if err != nil {
		t.Fatal(err)
	}
	if signingIdentity.Public != device.SigningPublicKey {
		t.Fatalf("signing public key = %q, want %q", signingIdentity.Public, device.SigningPublicKey)
	}
	stdout, stderr, err = executeForTest("--home", home, "devices", "list", "--json")
	if err != nil {
		t.Fatalf("devices list stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	if !strings.Contains(stdout, `"trust_state": "local"`) {
		t.Fatalf("devices list stdout = %q, want local trust state", stdout)
	}
	stdout, stderr, err = executeForTest("--home", home, "devices", "rename", device.ID, "renamed-local")
	if err != nil {
		t.Fatalf("devices rename stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	_, stderr, err = executeForTest("--home", home, "devices", "revoke", device.ID)
	if err == nil {
		t.Fatal("expected local device revoke refusal")
	}
	if !strings.Contains(stderr, "refusing to change local device trust state") {
		t.Fatalf("devices revoke stderr = %q, want local-device refusal", stderr)
	}
	for _, path := range []string{filepath.Join(home, "state.db"), filepath.Join(home, "config.yaml")} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), identity.Private) {
			t.Fatalf("%s contains private device identity", path)
		}
		if strings.Contains(string(raw), signingIdentity.Private) {
			t.Fatalf("%s contains private device signing identity", path)
		}
	}

	stdout, stderr, err = executeForTest("--home", home, "doctor")
	if err != nil {
		t.Fatalf("doctor stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	// PROD-02: doctor is now a graded report. A healthy workspace has the
	// device-key and DB-integrity checks present and zero errors.
	if !strings.Contains(stdout, "foreign_key_check") || !strings.Contains(stdout, "device key") || !strings.Contains(stdout, "device signing key") {
		t.Fatalf("doctor stdout = %q, want device key + db checks present", stdout)
	}
	if !strings.Contains(stdout, "0 error(s)") {
		t.Fatalf("doctor stdout = %q, want 0 errors on a healthy workspace", stdout)
	}
}

func TestScanDryRunAndAdoptStatus(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init", "--workspace-name", "personal"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}

	repo := filepath.Join(root, "work", "acme", "api")
	if err := os.MkdirAll(filepath.Join(repo, "node_modules", "huge"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("TOKEN=do-not-store"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "init")
	runGit(t, repo, "remote", "add", "origin", "git@github.com:acme/api.git")

	stdout, stderr, err := executeForTest("--home", home, "scan", root, "--dry-run", "--json")
	if err != nil {
		t.Fatalf("scan stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	var scanResult struct {
		Findings []struct {
			Path      string `json:"path"`
			RemoteKey string `json:"remote_key"`
		} `json:"findings"`
	}
	if err := json.Unmarshal([]byte(stdout), &scanResult); err != nil {
		t.Fatalf("scan --json is not valid JSON: %v\n%s", err, stdout)
	}
	var found bool
	for _, f := range scanResult.Findings {
		if f.Path == "work/acme/api" && f.RemoteKey == "github.com/acme/api" {
			found = true
		}
	}
	if !found {
		t.Fatalf("scan findings = %+v, want work/acme/api", scanResult.Findings)
	}
	if strings.Contains(stdout, "do-not-store") {
		t.Fatalf("scan leaked secret value: %s", stdout)
	}

	stdout, stderr, err = executeForTest("--home", home, "scan", root, "--adopt")
	if err != nil {
		t.Fatalf("adopt stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	if !strings.Contains(stdout, "Adopted 1 projects") {
		t.Fatalf("adopt stdout = %q", stdout)
	}
	stdout, stderr, err = executeForTest("--home", home, "sync", "--hub-file", filepath.Join(t.TempDir(), "hub.json"), "--dry-run")
	if err != nil {
		t.Fatalf("sync dry-run after adopt stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	if !strings.Contains(stdout, "Would push 1 local events") {
		t.Fatalf("sync dry-run stdout = %q, want adopted project event", stdout)
	}
	stdout, stderr, err = executeForTest("--home", home, "status", "--json")
	if err != nil {
		t.Fatalf("status stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	var adopted state.Summary
	if err := json.Unmarshal([]byte(stdout), &adopted); err != nil {
		t.Fatalf("status --json is not valid JSON: %v\n%s", err, stdout)
	}
	if adopted.ProjectCount != 1 {
		t.Fatalf("project_count = %d, want 1", adopted.ProjectCount)
	}
	if len(adopted.Projects) != 1 || adopted.Projects[0].Path != "work/acme/api" {
		t.Fatalf("status projects = %+v, want adopted work/acme/api", adopted.Projects)
	}
}

func TestScanAdoptRefusesNonWorkspaceRoot(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init", "--workspace-name", "personal"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}

	other := filepath.Join(t.TempDir(), "Downloads")
	repo := filepath.Join(other, "stray")
	runGit(t, repo, "init")
	runGit(t, repo, "remote", "add", "origin", "git@github.com:acme/stray.git")

	stdout, stderr, err := executeForTest("--home", home, "scan", other, "--adopt")
	if err == nil {
		t.Fatalf("scan stdout = %q stderr = %q, want error", stdout, stderr)
	}
	var app appError
	if !errors.As(err, &app) || app.code != exitUsage {
		t.Fatalf("scan err = %v, want exitUsage appError", err)
	}
	if !strings.Contains(err.Error(), "--adopt only adopts from the workspace root") {
		t.Fatalf("scan err = %v, want workspace-root adoption refusal", err)
	}

	stdout, stderr, err = executeForTest("--home", home, "status", "--json")
	if err != nil {
		t.Fatalf("status stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	var summary state.Summary
	if err := json.Unmarshal([]byte(stdout), &summary); err != nil {
		t.Fatalf("status --json is not valid JSON: %v\n%s", err, stdout)
	}
	if summary.ProjectCount != 0 {
		t.Fatalf("project_count = %d, want 0", summary.ProjectCount)
	}
}

func TestScanAdoptExplicitWorkspaceRootSucceeds(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init", "--workspace-name", "personal"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}

	repo := filepath.Join(root, "work", "acme", "api")
	runGit(t, repo, "init")
	runGit(t, repo, "remote", "add", "origin", "git@github.com:acme/api.git")

	stdout, stderr, err := executeForTest("--home", home, "scan", root, "--adopt")
	if err != nil {
		t.Fatalf("adopt stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	if !strings.Contains(stdout, "Adopted 1 projects") {
		t.Fatalf("adopt stdout = %q", stdout)
	}
}

// A symlink alias of the workspace root names the same directory, so --adopt
// must accept it (P6-CLI-02 review: EvalSymlinks-based comparison), and the
// adopted local paths must use the canonical root spelling, not the alias.
func TestScanAdoptAcceptsSymlinkedWorkspaceRoot(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init", "--workspace-name", "personal"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}

	repo := filepath.Join(root, "work", "acme", "api")
	runGit(t, repo, "init")
	runGit(t, repo, "remote", "add", "origin", "git@github.com:acme/api.git")

	alias := filepath.Join(t.TempDir(), "CodeAlias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	stdout, stderr, err := executeForTest("--home", home, "scan", alias, "--adopt")
	if err != nil {
		t.Fatalf("adopt via symlink alias stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	if !strings.Contains(stdout, "Adopted 1 projects") {
		t.Fatalf("adopt stdout = %q", stdout)
	}
}

func TestScanReadOnlyAllowsNonWorkspaceRoot(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init", "--workspace-name", "personal"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}

	other := filepath.Join(t.TempDir(), "Downloads")
	repo := filepath.Join(other, "stray")
	runGit(t, repo, "init")
	runGit(t, repo, "remote", "add", "origin", "git@github.com:acme/stray.git")

	stdout, stderr, err := executeForTest("--home", home, "scan", other)
	if err != nil {
		t.Fatalf("scan stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	if !strings.Contains(stdout, "stray\tgit_repo") {
		t.Fatalf("scan stdout = %q, want read-only finding", stdout)
	}
}

func TestEnvCaptureEncryptsBindingsAndDoesNotPersistPlaintext(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	remote := filepath.Join(t.TempDir(), "repo.git")
	runGit(t, filepath.Dir(remote), "init", "--bare", remote)
	repo := filepath.Join(root, "work", "acme", "api")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "init")
	runGit(t, repo, "remote", "add", "origin", "file://"+remote)
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("API_TOKEN=abc123\nQUOTED=\"two words # kept\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := executeForTest("--home", home, "scan", root, "--adopt"); err != nil {
		t.Fatalf("scan stderr = %q err = %v", stderr, err)
	}
	remoteDevice, err := devicekeys.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	remoteSigning, err := devicekeys.NewSigningIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if stdout, stderr, err := executeForTest("--home", home, "devices", "enroll", "dev_remote", "--name", "remote", "--os", "linux", "--arch", "arm64", "--age-recipient", remoteDevice.Recipient, "--signing-public-key", remoteSigning.Public, "--approve"); err != nil {
		t.Fatalf("device enroll stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	stdout, stderr, err := executeForTest("--home", home, "env", "capture", "work/acme/api", ".env")
	if err != nil {
		t.Fatalf("env capture stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	if !strings.Contains(stdout, "Captured 2 env variables") || !strings.Contains(stdout, "age_blob:") || !strings.Contains(stdout, "2 recipient device(s)") {
		t.Fatalf("env capture stdout = %q, want capture summary", stdout)
	}

	store, err := state.Open(context.Background(), filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(store)
	project, err := store.ProjectByPath(t.Context(), "work/acme/api")
	if err != nil {
		t.Fatal(err)
	}
	profile, bindings, err := store.EnvProfileForProject(t.Context(), project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Provider != "devstrap_encrypted" || profile.Mode != "hydrate_or_runtime" {
		t.Fatalf("profile = %+v, want encrypted hydrate/runtime profile", profile)
	}
	if len(bindings) != 2 {
		t.Fatalf("bindings = %+v, want two", bindings)
	}
	ref := bindings[0].EncryptedValueRef
	for _, binding := range bindings {
		if binding.EncryptedValueRef == "" || binding.EncryptedValueRef != ref {
			t.Fatalf("binding = %+v, want shared encrypted blob ref %q", binding, ref)
		}
	}
	hash := strings.TrimPrefix(ref, "age_blob:")
	blobPath := filepath.Join(home, "blobs", hash+".age")
	info, err := os.Stat(blobPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("blob mode = %s, want 0600", info.Mode().Perm())
	}
	blobRaw, err := os.ReadFile(blobPath)
	if err != nil {
		t.Fatal(err)
	}
	remotePlaintext, err := envbundle.Decrypt(blobRaw, remoteDevice.Private)
	if err != nil {
		t.Fatalf("approved remote device could not decrypt env blob: %v", err)
	}
	if len(remotePlaintext.Vars) != 2 {
		t.Fatalf("remote plaintext vars = %+v, want two", remotePlaintext.Vars)
	}
	for _, path := range []string{filepath.Join(home, "state.db"), filepath.Join(home, "config.yaml"), blobPath} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "abc123") || strings.Contains(string(raw), "two words") {
			t.Fatalf("%s contains captured plaintext", path)
		}
	}
	gitignore, err := os.ReadFile(filepath.Join(repo, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(gitignore), ".env\n") {
		t.Fatalf(".gitignore = %q, want captured env file ignored", gitignore)
	}

	stdout, stderr, err = executeForTest("--home", home, "run", "work/acme/api", "--", "sh", "-c", "printf '%s|%s' \"$API_TOKEN\" \"$QUOTED\"")
	if err != nil {
		t.Fatalf("encrypted env run stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	if stdout != "abc123|two words # kept" {
		t.Fatalf("encrypted env run stdout = %q, want decrypted env values", stdout)
	}

	stdout, stderr, err = executeForTest("--home", home, "env", "hydrate", "work/acme/api", "--write", ".env.local")
	if err != nil {
		t.Fatalf("env hydrate stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	if !strings.Contains(stdout, "Hydrated 2 env variables") {
		t.Fatalf("env hydrate stdout = %q, want hydrate summary", stdout)
	}
	hydratedPath := filepath.Join(repo, ".env.local")
	info, err = os.Stat(hydratedPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf(".env.local mode = %s, want 0600", info.Mode().Perm())
	}
	hydrated, err := os.ReadFile(hydratedPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(hydrated), `API_TOKEN='abc123'`) || !strings.Contains(string(hydrated), `QUOTED='two words # kept'`) {
		t.Fatalf(".env.local = %q, want hydrated values", hydrated)
	}
	gitignore, err = os.ReadFile(filepath.Join(repo, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(gitignore), ".env.local\n") {
		t.Fatalf(".gitignore = %q, want hydrated env file ignored", gitignore)
	}
	_, stderr, err = executeForTest("--home", home, "env", "hydrate", "work/acme/api", "--write", ".env.local")
	if err == nil {
		t.Fatal("expected hydrate overwrite refusal")
	}
	if !strings.Contains(stderr, "pass --force") {
		t.Fatalf("env hydrate stderr = %q, want force hint", stderr)
	}
	stdout, stderr, err = executeForTest("--home", home, "env", "hydrate", "work/acme/api", "--write", ".env.local", "--force")
	if err != nil {
		t.Fatalf("env hydrate force stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}

	fakeBin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeOP := filepath.Join(fakeBin, "op")
	fakeScript := `#!/bin/sh
if [ "$1" = "inject" ]; then
  shift
  in=""
  out=""
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --in-file)
        in="$2"
        shift 2
        ;;
      --out-file)
        out="$2"
        shift 2
        ;;
      --file-mode|--force)
        if [ "$1" = "--file-mode" ]; then shift 2; else shift; fi
        ;;
      *)
        shift
        ;;
    esac
  done
  sed 's#op://dev/api/token#resolved-token#g; s#op://dev/api/quoted#resolved quoted#g' "$in" > "$out"
  chmod 600 "$out"
  exit 0
fi
while [ "$#" -gt 0 ]; do
  case "$1" in
    --env-file)
      echo "env-file:"
      cat "$2"
      shift 2
      ;;
    --)
      shift
      echo "cmd:$*"
      exit 0
      ;;
    *)
      shift
      ;;
  esac
done
`
	if err := os.WriteFile(fakeOP, []byte(fakeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("OP_SERVICE_ACCOUNT_TOKEN", "op-token-for-test")
	refsPath := filepath.Join(repo, ".env.refs")
	refsContent := "API_TOKEN=op://dev/api/token\nQUOTED=\"op://dev/api/quoted\"\n"
	if err := os.WriteFile(refsPath, []byte(refsContent), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err = executeForTest("--home", home, "env", "bind", "work/acme/api", ".env.refs", "--provider", "1password")
	if err != nil {
		t.Fatalf("env bind stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	if !strings.Contains(stdout, "Bound 2 provider refs") {
		t.Fatalf("env bind stdout = %q, want bind summary", stdout)
	}
	stdout, stderr, err = executeForTest("--home", home, "run", "work/acme/api", "--", "printenv", "API_TOKEN")
	if err != nil {
		t.Fatalf("provider env run stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	if !strings.Contains(stdout, `API_TOKEN='op://dev/api/token'`) || !strings.Contains(stdout, "cmd:printenv API_TOKEN") {
		t.Fatalf("provider env run stdout = %q, want op refs file and command args", stdout)
	}
	providerHydratePath := filepath.Join(repo, ".env.provider.local")
	stdout, stderr, err = executeForTest("--home", home, "env", "hydrate", "work/acme/api", "--write", ".env.provider.local")
	if err != nil {
		t.Fatalf("provider env hydrate stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	if !strings.Contains(stdout, "Hydrated 2 env variables") {
		t.Fatalf("provider env hydrate stdout = %q, want hydrate summary", stdout)
	}
	providerHydrated, err := os.ReadFile(providerHydratePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(providerHydrated), `API_TOKEN='resolved-token'`) || !strings.Contains(string(providerHydrated), `QUOTED='resolved quoted'`) {
		t.Fatalf(".env.provider.local = %q, want injected provider values", providerHydrated)
	}
	info, err = os.Stat(providerHydratePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf(".env.provider.local mode = %s, want 0600", info.Mode().Perm())
	}
	_, stderr, err = executeForTest("--home", home, "env", "hydrate", "work/acme/api", "--write", ".env.provider.local")
	if err == nil {
		t.Fatal("expected provider hydrate overwrite refusal")
	}
	if !strings.Contains(stderr, "pass --force") {
		t.Fatalf("provider env hydrate stderr = %q, want force hint", stderr)
	}
	gitignore, err = os.ReadFile(filepath.Join(repo, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(gitignore), ".env.refs\n") || !strings.Contains(string(gitignore), ".env.provider.local\n") {
		t.Fatalf(".gitignore = %q, want provider refs file ignored", gitignore)
	}
}

func TestAddHydrateLocalBareRemoteAndRefuseDirtyTarget(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	remote := filepath.Join(t.TempDir(), "repo.git")
	runGit(t, filepath.Dir(remote), "init", "--bare", remote)

	stdout, stderr, err := executeForTest("--home", home, "add", "file://"+remote, "--path", "work/acme/repo")
	if err != nil {
		t.Fatalf("add stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	if _, err := os.Stat(filepath.Join(root, "work", "acme", "repo", ".devstrap", "placeholder.json")); err != nil {
		t.Fatalf("skeleton missing: %v", err)
	}
	stdout, stderr, err = executeForTest("--home", home, "hydrate", "work/acme/repo")
	if err != nil {
		t.Fatalf("hydrate stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	if _, err := os.Stat(filepath.Join(root, "work", "acme", "repo", ".git")); err != nil {
		t.Fatalf("hydrated repo missing .git: %v", err)
	}

	dirtyPath := filepath.Join(root, "work", "acme", "dirty")
	if err := os.MkdirAll(dirtyPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirtyPath, "notes.txt"), []byte("local"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, stderr, err = executeForTest("--home", home, "add", "file://"+remote, "--path", "work/acme/dirty")
	if err == nil {
		t.Fatal("expected dirty target refusal")
	}
	if !strings.Contains(stderr, "refusing to hydrate into non-empty directory") {
		t.Fatalf("stderr = %q, want dirty target refusal", stderr)
	}

	missingRemote := filepath.Join(t.TempDir(), "missing.git")
	stdout, stderr, err = executeForTest("--home", home, "add", "file://"+missingRemote, "--path", "work/acme/missing")
	if err != nil {
		t.Fatalf("add missing remote stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	missingPath := filepath.Join(root, "work", "acme", "missing")
	placeholderPath := filepath.Join(missingPath, ".devstrap", "placeholder.json")
	if _, err := os.Stat(placeholderPath); err != nil {
		t.Fatalf("missing remote skeleton placeholder missing before hydrate: %v", err)
	}
	_, stderr, err = executeForTest("--home", home, "hydrate", "work/acme/missing")
	if err == nil {
		t.Fatal("expected hydrate to fail for missing remote")
	}
	if _, statErr := os.Stat(placeholderPath); statErr != nil {
		t.Fatalf("missing remote hydrate removed original skeleton: %v; stderr=%q", statErr, stderr)
	}
	tmpMatches, err := filepath.Glob(filepath.Join(filepath.Dir(missingPath), "."+filepath.Base(missingPath)+".devstrap-tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tmpMatches) != 0 {
		t.Fatalf("leftover hydrate temp dirs = %v", tmpMatches)
	}
}

func TestAddRejectsInvalidLFSPolicy(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	remote := filepath.Join(t.TempDir(), "repo.git")
	runGit(t, filepath.Dir(remote), "init", "--bare", remote)
	_, stderr, err := executeForTest("--home", home, "add", "file://"+remote, "--path", "work/acme/repo", "--lfs-policy", "surprise")
	if err == nil {
		t.Fatal("expected invalid LFS policy error")
	}
	if !strings.Contains(stderr, "unsupported lfs policy") {
		t.Fatalf("stderr = %q, want unsupported lfs policy", stderr)
	}
}

func TestPromoteClonedRepoRefusesLateDirtyTarget(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "work", "acme", "repo")
	if err := writeSkeleton(target, "work/acme/repo", "git@example.com:acme/repo.git"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "notes.txt"), []byte("late local file"), 0o644); err != nil {
		t.Fatal(err)
	}
	tmp, err := cloneTempDir(target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	if err := os.WriteFile(filepath.Join(tmp, "README.md"), []byte("cloned"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = promoteClonedRepo(tmp, target, "work/acme/repo", "git@example.com:acme/repo.git")
	if err == nil {
		t.Fatal("expected dirty target promotion refusal")
	}
	if !strings.Contains(err.Error(), "refusing to hydrate into non-empty directory") {
		t.Fatalf("promote error = %v, want dirty target refusal", err)
	}
	if _, err := os.Stat(filepath.Join(target, "notes.txt")); err != nil {
		t.Fatalf("dirty target file was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "README.md")); err != nil {
		t.Fatalf("staged clone was removed by promote helper: %v", err)
	}
}

func TestRepoLockRejectsActiveAndReclaimsStaleOwner(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	unlock, err := acquireRepoLock(home, "prj_test")
	if err != nil {
		t.Fatal(err)
	}
	_, err = acquireRepoLock(home, "prj_test")
	if err == nil {
		t.Fatal("expected active lock conflict")
	}
	if !strings.Contains(err.Error(), "repo operation already in progress") {
		t.Fatalf("active lock error = %v", err)
	}
	unlock()

	oldAlive := repoLockProcessAlive
	oldStaleAfter := repoLockStaleAfter
	repoLockProcessAlive = func(pid int) bool { return false }
	repoLockStaleAfter = time.Hour
	t.Cleanup(func() {
		repoLockProcessAlive = oldAlive
		repoLockStaleAfter = oldStaleAfter
	})

	lockDir := filepath.Join(home, "locks")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	staleInfo := `{"pid":999999,"hostname":"` + hostname() + `","acquired_at":"` + time.Now().UTC().Format(time.RFC3339Nano) + `"}`
	if err := os.WriteFile(filepath.Join(lockDir, "prj_test.lock"), []byte(staleInfo), 0o600); err != nil {
		t.Fatal(err)
	}
	unlock, err = acquireRepoLock(home, "prj_test")
	if err != nil {
		t.Fatalf("stale lock was not reclaimed: %v", err)
	}
	unlock()
}

func TestWorktreeNewUsesFreshRemoteDefaultSHA(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}

	tmp := t.TempDir()
	remote := filepath.Join(tmp, "repo.git")
	seed := filepath.Join(tmp, "seed")
	runGit(t, tmp, "init", "--bare", remote)
	runGit(t, seed, "init")
	runGit(t, seed, "config", "user.email", "devstrap@example.test")
	runGit(t, seed, "config", "user.name", "DevStrap Test")
	runGit(t, seed, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, ".gitattributes"), []byte("*.bin filter=lfs diff=lfs merge=lfs -text\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, seed, "add", "README.md", ".gitattributes")
	runGit(t, seed, "commit", "-m", "initial")
	runGit(t, seed, "remote", "add", "origin", remote)
	runGit(t, seed, "push", "origin", "main")
	runGit(t, tmp, "--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/main")

	if _, stderr, err := executeForTest("--home", home, "add", "file://"+remote, "--path", "work/acme/repo", "--default-branch", "main"); err != nil {
		t.Fatalf("add stderr = %q err = %v", stderr, err)
	}
	if _, stderr, err := executeForTest("--home", home, "hydrate", "work/acme/repo"); err != nil {
		t.Fatalf("hydrate stderr = %q err = %v", stderr, err)
	}

	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, seed, "commit", "-am", "advance")
	runGit(t, seed, "push", "origin", "main")
	latest := strings.TrimSpace(runGitOutput(t, seed, "rev-parse", "HEAD"))

	stdout, stderr, err := executeForTest("--home", home, "worktree", "new", "work/acme/repo", "--fresh-upstream", "--name", "fresh base")
	if err != nil {
		t.Fatalf("worktree stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	if !strings.Contains(stdout, latest) {
		t.Fatalf("worktree stdout = %q, want latest remote SHA %s", stdout, latest)
	}
	if !strings.Contains(stdout, "uses Git LFS") || !strings.Contains(stdout, "lfs_policy=auto") {
		t.Fatalf("worktree stdout = %q, want LFS pointer warning", stdout)
	}

	stdout, stderr, err = executeForTest("--home", home, "worktree", "list", "--json")
	if err != nil {
		t.Fatalf("worktree list stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	var worktrees []struct {
		ID      string `json:"id"`
		Path    string `json:"path"`
		BaseSHA string `json:"base_sha"`
	}
	if err := json.Unmarshal([]byte(stdout), &worktrees); err != nil {
		t.Fatalf("decode worktree list: %v\n%s", err, stdout)
	}
	if len(worktrees) != 1 || worktrees[0].ID == "" || worktrees[0].Path == "" {
		t.Fatalf("worktree list = %+v, want one worktree with ID", worktrees)
	}

	// TEST-3: prove the worktree branched from the FETCHED remote default, not
	// the stale local clone. The local clone was hydrated before the "two"
	// commit, so its HEAD must differ from latest; the worktree's filesystem
	// HEAD and recorded base SHA must both equal latest.
	localClone := filepath.Join(root, "work", "acme", "repo")
	localHead := strings.TrimSpace(runGitOutput(t, localClone, "rev-parse", "HEAD"))
	if localHead == latest {
		t.Fatal("local clone was not stale; test no longer proves fresh-upstream behavior")
	}
	wtHead := strings.TrimSpace(runGitOutput(t, worktrees[0].Path, "rev-parse", "HEAD"))
	if wtHead != latest {
		t.Fatalf("worktree HEAD = %s, want fresh remote SHA %s", wtHead, latest)
	}
	if worktrees[0].BaseSHA != latest {
		t.Fatalf("recorded base SHA = %s, want fresh remote SHA %s", worktrees[0].BaseSHA, latest)
	}

	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("three\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, seed, "commit", "-am", "advance again")
	runGit(t, seed, "push", "origin", "main")

	stdout, stderr, err = executeForTest("--home", home, "worktree", "status", worktrees[0].ID)
	if err != nil {
		t.Fatalf("worktree status stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	if !strings.Contains(stdout, "stale (behind 1)") {
		t.Fatalf("worktree status stdout = %q, want stale base", stdout)
	}

	_, stderr, err = executeForTest("--home", home, "worktree", "finalize", worktrees[0].ID)
	if err == nil {
		t.Fatal("expected finalize to reject stale base")
	}
	if !strings.Contains(stderr, "base origin/main moved 1 commits") || !strings.Contains(stderr, "--allow-stale-base") {
		t.Fatalf("finalize stale stderr = %q, want stale-base refusal", stderr)
	}

	stdout, stderr, err = executeForTest("--home", home, "worktree", "finalize", worktrees[0].ID, "--allow-stale-base")
	if err != nil {
		t.Fatalf("finalize allow stale stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	if !strings.Contains(stdout, "Warning: finalizing stale worktree") {
		t.Fatalf("finalize allow stale stdout = %q, want warning", stdout)
	}

	if err := os.RemoveAll(worktrees[0].Path); err != nil {
		t.Fatal(err)
	}
	_, stderr, err = executeForTest("--home", home, "worktree", "remove", worktrees[0].ID)
	if err == nil {
		t.Fatal("expected missing worktree remove to require --force")
	}
	if !strings.Contains(stderr, "pass --force") {
		t.Fatalf("remove missing stderr = %q, want force hint", stderr)
	}
	stdout, stderr, err = executeForTest("--home", home, "worktree", "remove", worktrees[0].ID, "--force")
	if err != nil {
		t.Fatalf("remove missing force stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	if !strings.Contains(stdout, "Pruned missing worktree") {
		t.Fatalf("remove missing force stdout = %q, want prune message", stdout)
	}
	gitWorktrees := runGitOutput(t, filepath.Join(root, "work", "acme", "repo"), "worktree", "list")
	if strings.Contains(gitWorktrees, worktrees[0].Path) {
		t.Fatalf("git worktree list still contains removed path %s:\n%s", worktrees[0].Path, gitWorktrees)
	}
	stdout, stderr, err = executeForTest("--home", home, "worktree", "list", "--json")
	if err != nil {
		t.Fatalf("worktree list after remove stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	if strings.Contains(stdout, worktrees[0].ID) {
		t.Fatalf("removed worktree still listed: %s", stdout)
	}
}

func TestAgentRunRecordsLogsDiffAndPRStaleGate(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}

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
	runGit(t, seed, "commit", "-m", "initial")
	runGit(t, seed, "remote", "add", "origin", remote)
	runGit(t, seed, "push", "origin", "main")
	runGit(t, tmp, "--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/main")

	if _, stderr, err := executeForTest("--home", home, "add", "file://"+remote, "--path", "work/acme/agent-repo", "--default-branch", "main"); err != nil {
		t.Fatalf("add stderr = %q err = %v", stderr, err)
	}
	_, stderr, err := executeForTest("--home", home, "agent", "run", "work/acme/agent-repo", "--engine", "generic", "--task", "leak env", "--", "sh", "-c", "cat .env")
	if err == nil {
		t.Fatal("expected guarded agent policy refusal")
	}
	if !strings.Contains(stderr, "agent policy guarded denied") {
		t.Fatalf("agent policy stderr = %q, want guarded denial", stderr)
	}
	_, stderr, err = executeForTest("--home", home, "agent", "run", "work/acme/agent-repo", "--engine", "generic", "--task", "outside read", "--", "cat", filepath.Join(t.TempDir(), "outside.txt"))
	if err == nil {
		t.Fatal("expected guarded agent file policy refusal")
	}
	if !strings.Contains(stderr, "agent file policy guarded denied path outside worktree") {
		t.Fatalf("agent file policy stderr = %q, want outside-worktree denial", stderr)
	}
	_, stderr, err = executeForTest("--home", home, "agent", "run", "work/acme/agent-repo", "--engine", "generic", "--task", "ssh key read", "--", "cat", filepath.Join(home, ".ssh", "id_ed25519"))
	if err == nil {
		t.Fatal("expected guarded agent sensitive-path refusal")
	}
	if !strings.Contains(stderr, "agent file policy guarded denied sensitive path") {
		t.Fatalf("agent sensitive-path stderr = %q, want sensitive-path denial", stderr)
	}
	stdout, stderr, err := executeForTest("--home", home, "agent", "run", "work/acme/agent-repo", "--engine", "generic", "--task", "write agent file", "--", "touch", "agent.txt")
	if err != nil {
		t.Fatalf("agent run stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	if !strings.Contains(stdout, "Agent run") || !strings.Contains(stdout, "complete") || !strings.Contains(stdout, "agent.txt") {
		t.Fatalf("agent run stdout = %q, want completion and diff summary", stdout)
	}

	stdout, stderr, err = executeForTest("--home", home, "agent", "list", "--json")
	if err != nil {
		t.Fatalf("agent list stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	var runs []state.AgentRun
	if err := json.Unmarshal([]byte(stdout), &runs); err != nil {
		t.Fatalf("agent list JSON = %q: %v", stdout, err)
	}
	if len(runs) != 1 {
		t.Fatalf("agent runs = %+v, want one", runs)
	}
	if runs[0].Status != "complete" || runs[0].WorktreeID == "" || !strings.Contains(runs[0].DiffSummary, "agent.txt") {
		t.Fatalf("agent run = %+v, want complete run with diff summary", runs[0])
	}
	logRaw, err := os.ReadFile(runs[0].LogPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logRaw), "OP_SERVICE_ACCOUNT_TOKEN") {
		t.Fatalf("agent log leaked provider env: %s", logRaw)
	}

	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("advanced\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, seed, "commit", "-am", "advance base")
	runGit(t, seed, "push", "origin", "main")
	_, stderr, err = executeForTest("--home", home, "agent", "pr", runs[0].ID, "--dry-run")
	if err == nil {
		t.Fatal("expected stale-base refusal before agent PR")
	}
	if !strings.Contains(stderr, "base origin/main moved") {
		t.Fatalf("agent pr stderr = %q, want stale-base gate", stderr)
	}
	stdout, stderr, err = executeForTest("--home", home, "agent", "pr", runs[0].ID, "--dry-run", "--allow-stale-base")
	if err != nil {
		t.Fatalf("agent pr dry-run stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	if !strings.Contains(stdout, "Would create PR") {
		t.Fatalf("agent pr dry-run stdout = %q, want dry-run summary", stdout)
	}
	fakeBin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	ghArgsPath := filepath.Join(t.TempDir(), "gh-args.txt")
	fakeGH := filepath.Join(fakeBin, "gh")
	script := "#!/bin/sh\n{\n  pwd\n  printf '%s\\n' \"$@\"\n} > " + ghArgsPath + "\nprintf 'https://github.com/acme/repo/pull/123\\n'\n"
	if err := os.WriteFile(fakeGH, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	stdout, stderr, err = executeForTest("--home", home, "agent", "pr", runs[0].ID, "--allow-stale-base", "--title", "Agent PR", "--body", "Body")
	if err != nil {
		t.Fatalf("agent pr create stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	// FORGE-01: with a file:// remote, the forge is not auto-detected, so the
	// PR path degrades gracefully (branch pushed + compare URL) instead of
	// calling gh. The stale-base gate and push are still verified.
	if !strings.Contains(stdout, "pushed") {
		t.Fatalf("agent pr create stdout = %q, want forge-agnostic push message", stdout)
	}
	// Note: gh args verification is skipped for file:// remotes because the
	// forge-agnostic path does not call gh. A separate test with a
	// GitHub-like remote URL would verify gh pr create argv.

	// GIT-05: --forge override routes a self-hosted-looking (file://) remote
	// to glab even though DetectForge cannot infer it, and the fake glab is
	// invoked with the mr-create argv.
	glabArgsPath := filepath.Join(t.TempDir(), "glab-args.txt")
	fakeGlab := filepath.Join(fakeBin, "glab")
	glabScript := "#!/bin/sh\n{\n  pwd\n  printf '%s\\n' \"$@\"\n} > " + glabArgsPath + "\nprintf 'https://gitlab.acme.com/acme/repo/-/merge_requests/7\\n'\n"
	if err := os.WriteFile(fakeGlab, []byte(glabScript), 0o755); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err = executeForTest("--home", home, "agent", "pr", runs[0].ID, "--allow-stale-base", "--forge", "gitlab", "--title", "Self-hosted MR", "--body", "Body")
	if err != nil {
		t.Fatalf("agent pr --forge gitlab stdout = %q stderr = %q err = %v", stdout, stderr, err)
	}
	if !strings.Contains(stdout, "merge_requests/7") {
		t.Fatalf("agent pr --forge gitlab stdout = %q, want glab MR URL", stdout)
	}
	glabArgs, err := os.ReadFile(glabArgsPath)
	if err != nil {
		t.Fatalf("glab not invoked: %v", err)
	}
	if !strings.Contains(string(glabArgs), "mr") || !strings.Contains(string(glabArgs), "create") {
		t.Fatalf("glab argv = %q, want mr create", glabArgs)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}

func runGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
	return string(out)
}

// P6-GIT-01 review (CodeRabbit): a malformed clone_timeout must fall back to
// the default, never silently become "no timeout"; an explicit "0" stays the
// documented unbounded opt-out.
func TestGitRunnerCloneTimeoutValidation(t *testing.T) {
	cases := []struct {
		raw  string
		want time.Duration
	}{
		{"3 hours", 30 * time.Minute}, // malformed → default, not unbounded
		{"-5m", 30 * time.Minute},     // negative → default
		{"0", 0},                      // explicit unbounded opt-out
		{"45m", 45 * time.Minute},
	}
	for _, tc := range cases {
		v := viper.New()
		v.Set("materialization.clone_timeout", tc.raw)
		if got := gitRunner(&options{v: v}).LongTimeout; got != tc.want {
			t.Fatalf("clone_timeout %q: LongTimeout = %s, want %s", tc.raw, got, tc.want)
		}
	}
}
