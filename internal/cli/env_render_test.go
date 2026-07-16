package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/platform"
	"github.com/Reederey87/DevStrap/internal/state"
	"github.com/Reederey87/DevStrap/internal/workspacekeys"
	"github.com/spf13/viper"
)

// P5-CLI-01 part B: env capture/rotate/hydrate/bind, draft snapshot create, and
// keys rotate --json shapes via the shared opts.render seam. Every test also
// asserts that known plaintext secret material never appears in --json stdout.

const secretLeakMarker = "super-secret-value-12345"

func setupEnvJSONProject(t *testing.T, home, root, secret string) {
	t.Helper()
	ctx := context.Background()
	store, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertProject(ctx, state.UpsertProjectParams{
		Path:      "work/proj",
		Type:      "git_repo",
		RemoteKey: "github.com/acme/proj",
		RemoteURL: "https://github.com/acme/proj",
	}); err != nil {
		closeStore(store)
		t.Fatal(err)
	}
	closeStore(store)

	projDir := filepath.Join(root, "work", "proj")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projDir, ".env"), []byte("SECRET_TOKEN="+secret+"\nOTHER=ok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertNoSecretLeak(t *testing.T, stdout, stderr, secret string) {
	t.Helper()
	if strings.Contains(stdout, secret) || strings.Contains(stderr, secret) {
		t.Fatalf("secret material leaked into CLI output: stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestEnvCaptureJSON(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	setupEnvJSONProject(t, home, root, secretLeakMarker)

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json",
		"env", "capture", "work/proj", ".env")
	if err != nil {
		t.Fatalf("env capture --json stderr = %q err = %v", stderr, err)
	}
	assertNoSecretLeak(t, stdout, stderr, secretLeakMarker)

	var got envCaptureResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("env capture --json is not envCaptureResult: %v\n%s", err, stdout)
	}
	if got.Path != "work/proj" {
		t.Fatalf("path = %q, want work/proj", got.Path)
	}
	if !strings.HasPrefix(got.Ref, "age_blob:") {
		t.Fatalf("ref = %q, want age_blob:…", got.Ref)
	}
	if got.Bindings != 2 {
		t.Fatalf("bindings = %d, want 2", got.Bindings)
	}
	if got.Recipients < 1 {
		t.Fatalf("recipients = %d, want >= 1", got.Recipients)
	}
}

func TestEnvRotateJSONAll(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	ctx := context.Background()
	store, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertProject(ctx, state.UpsertProjectParams{
		Path: "work/x", Type: "git_repo", RemoteKey: "github.com/acme/x", RemoteURL: "https://github.com/acme/x",
	}); err != nil {
		closeStore(store)
		t.Fatal(err)
	}
	proj, err := store.ProjectByPath(ctx, "work/x")
	if err != nil {
		closeStore(store)
		t.Fatal(err)
	}
	if _, err := store.SaveCapturedEnvProfile(ctx, proj.ID, "default", []string{"API_KEY"}, "age_blob:"+hex64a); err != nil {
		closeStore(store)
		t.Fatal(err)
	}
	if _, err := store.MarkEncryptedBindingsNeedingRotation(ctx); err != nil {
		closeStore(store)
		t.Fatal(err)
	}
	closeStore(store)

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json", "env", "rotate", "--all")
	if err != nil {
		t.Fatalf("env rotate --all --json stderr = %q err = %v", stderr, err)
	}
	assertNoSecretLeak(t, stdout, stderr, secretLeakMarker)

	var got envRotateResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("env rotate --all --json is not envRotateResult: %v\n%s", err, stdout)
	}
	if got.Cleared != 1 {
		t.Fatalf("cleared = %d, want 1", got.Cleared)
	}
	if got.Path != "" || got.Ref != "" || got.Recaptured != 0 {
		t.Fatalf("unexpected recapture fields on --all: %+v", got)
	}
}

func TestEnvRotateJSONRecapture(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	setupEnvJSONProject(t, home, root, secretLeakMarker)

	// Seed a profile and flag it so the clear step is meaningful.
	ctx := context.Background()
	store, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	proj, err := store.ProjectByPath(ctx, "work/proj")
	if err != nil {
		closeStore(store)
		t.Fatal(err)
	}
	if _, err := store.SaveCapturedEnvProfile(ctx, proj.ID, "default", []string{"SECRET_TOKEN", "OTHER"}, "age_blob:"+hex64a); err != nil {
		closeStore(store)
		t.Fatal(err)
	}
	if _, err := store.MarkEncryptedBindingsNeedingRotation(ctx); err != nil {
		closeStore(store)
		t.Fatal(err)
	}
	closeStore(store)

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json",
		"env", "rotate", "work/proj", ".env")
	if err != nil {
		t.Fatalf("env rotate recapture --json stderr = %q err = %v", stderr, err)
	}
	assertNoSecretLeak(t, stdout, stderr, secretLeakMarker)

	var got envRotateResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("env rotate recapture --json is not envRotateResult: %v\n%s", err, stdout)
	}
	if got.Path != "work/proj" {
		t.Fatalf("path = %q, want work/proj", got.Path)
	}
	if !strings.HasPrefix(got.Ref, "age_blob:") {
		t.Fatalf("ref = %q, want age_blob:…", got.Ref)
	}
	if got.Recaptured != 2 {
		t.Fatalf("recaptured = %d, want 2", got.Recaptured)
	}
	if got.Recipients < 1 {
		t.Fatalf("recipients = %d, want >= 1", got.Recipients)
	}
	if got.Cleared < 1 {
		t.Fatalf("cleared = %d, want >= 1", got.Cleared)
	}
}

func TestEnvHydrateJSON(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	setupEnvJSONProject(t, home, root, secretLeakMarker)

	if _, stderr, err := executeForTest("--home", home, "--root", root,
		"env", "capture", "work/proj", ".env"); err != nil {
		t.Fatalf("env capture stderr = %q err = %v", stderr, err)
	}

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json",
		"env", "hydrate", "work/proj", "--write", ".env.hydrated")
	if err != nil {
		t.Fatalf("env hydrate --json stderr = %q err = %v", stderr, err)
	}
	assertNoSecretLeak(t, stdout, stderr, secretLeakMarker)

	var got envHydrateResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("env hydrate --json is not envHydrateResult: %v\n%s", err, stdout)
	}
	if got.Path != "work/proj" {
		t.Fatalf("path = %q, want work/proj", got.Path)
	}
	if got.Variables != 2 {
		t.Fatalf("variables = %d, want 2", got.Variables)
	}
	if !strings.HasSuffix(got.Target, filepath.Join("work", "proj", ".env.hydrated")) &&
		!strings.Contains(got.Target, ".env.hydrated") {
		t.Fatalf("target = %q, want path ending in .env.hydrated", got.Target)
	}

	// Plaintext lands in the target file (by design), never in --json stdout.
	raw, err := os.ReadFile(filepath.Join(root, "work", "proj", ".env.hydrated"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), secretLeakMarker) {
		t.Fatalf("hydrated file missing secret (setup broken): %q", raw)
	}
}

func TestEnvBindJSON(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	setupEnvJSONProject(t, home, root, secretLeakMarker)

	projDir := filepath.Join(root, "work", "proj")
	refsPath := filepath.Join(projDir, ".env.refs")
	// op:// pointers are references, not resolved secrets — still assert the
	// map values themselves are not dumped into the JSON result (count only).
	refsContent := "API_TOKEN=op://vault/item/field\nDB_URL=op://vault/item/db\n"
	if err := os.WriteFile(refsPath, []byte(refsContent), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json",
		"env", "bind", "work/proj", ".env.refs", "--provider", "1password")
	if err != nil {
		t.Fatalf("env bind --json stderr = %q err = %v", stderr, err)
	}
	assertNoSecretLeak(t, stdout, stderr, secretLeakMarker)
	if strings.Contains(stdout, "op://vault/item/field") || strings.Contains(stdout, "op://vault/item/db") {
		t.Fatalf("env bind --json leaked provider ref values: %s", stdout)
	}

	var got envBindResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("env bind --json is not envBindResult: %v\n%s", err, stdout)
	}
	if got.Path != "work/proj" || got.Provider != "1password" || got.Refs != 2 {
		t.Fatalf("env bind --json = %+v, want path=work/proj provider=1password refs=2", got)
	}
}

func TestDraftSnapshotCreateJSON(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	local := filepath.Join(root, "work", "local")
	// A remoteless local git repo classifies as a local-only/draft project
	// (NOVCS-01), not git_repo — matches the precedent in
	// TestDraftSnapshotCreateRecordsOriginSnapshotRow. A bare non-git folder
	// with no go.mod/package.json/README marker file (scan.looksLikeProject)
	// is never discovered as a candidate at all, which the first draft of this
	// test got wrong (it left the folder bare and scan silently adopted
	// nothing).
	runGit(t, local, "init")
	const draftSecret = "draft-secret-content-xyz"
	if err := os.WriteFile(filepath.Join(local, "note.txt"), []byte(draftSecret+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := executeForTest("--home", home, "--root", root, "scan", "--adopt"); err != nil {
		t.Fatalf("scan stderr = %q err = %v", stderr, err)
	}

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json",
		"draft", "snapshot", "create", "work/local")
	if err != nil {
		t.Fatalf("draft snapshot create --json stderr = %q err = %v", stderr, err)
	}
	if strings.Contains(stdout, draftSecret) {
		t.Fatalf("draft snapshot --json leaked file content: %s", stdout)
	}

	var got draftSnapshotResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("draft snapshot create --json is not draftSnapshotResult: %v\n%s", err, stdout)
	}
	if got.Path != "work/local" {
		t.Fatalf("path = %q, want work/local", got.Path)
	}
	if !strings.HasPrefix(got.BlobRef, "age_blob:") {
		t.Fatalf("blob_ref = %q, want age_blob:…", got.BlobRef)
	}
	if got.FileCount < 1 {
		t.Fatalf("file_count = %d, want >= 1", got.FileCount)
	}
	if got.ByteSize < 1 {
		t.Fatalf("byte_size = %d, want >= 1", got.ByteSize)
	}
	if got.Recipients < 1 {
		t.Fatalf("recipients = %d, want >= 1", got.Recipients)
	}
}

func TestKeysRotateJSON(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init: %v (%s)", err, stderr)
	}
	ctx := context.Background()
	opts := &options{v: viper.New()}
	opts.v.Set("home", home)
	opts.v.Set("root", root)
	st, err := state.Open(ctx, opts.paths().StateDB())
	if err != nil {
		t.Fatal(err)
	}
	keyring := workspacekeys.New(st, devicekeys.NewHybridStore(opts.paths().KeyDir(), platform.Detect().Keychain))
	if epoch, err := keyring.EnsureBootstrap(ctx); err != nil || epoch != 1 {
		closeStore(st)
		t.Fatalf("EnsureBootstrap = %d, %v; want epoch 1", epoch, err)
	}
	remoteAge, err := devicekeys.NewIdentity()
	if err != nil {
		closeStore(st)
		t.Fatal(err)
	}
	remoteSigning, err := devicekeys.NewSigningIdentity()
	if err != nil {
		closeStore(st)
		t.Fatal(err)
	}
	if err := st.UpsertDevice(ctx, state.Device{
		ID: "dev_rotate_json", Name: "peer", OS: "linux", Arch: "arm64",
		PublicKey: remoteAge.Recipient, SigningPublicKey: remoteSigning.Public, TrustState: "approved",
	}); err != nil {
		closeStore(st)
		t.Fatal(err)
	}
	closeStore(st)

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json", "keys", "rotate")
	if err != nil {
		t.Fatalf("keys rotate --json stderr = %q err = %v", stderr, err)
	}
	// No key material should appear; stderr may still carry a non-fatal
	// clearWCKRotationPending warning (human text only).
	var got keysRotateResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("keys rotate --json is not keysRotateResult: %v\nstdout=%q stderr=%q", err, stdout, stderr)
	}
	if got.Epoch != 2 {
		t.Fatalf("epoch = %d, want 2", got.Epoch)
	}
	if got.Grants < 1 {
		t.Fatalf("grants = %d, want >= 1", got.Grants)
	}
}
