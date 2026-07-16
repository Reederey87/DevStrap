package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/platform"
	"github.com/Reederey87/DevStrap/internal/redact"
	"github.com/spf13/viper"
)

func TestParseHubURI(t *testing.T) {
	cases := []struct {
		uri      string
		wantSpec hubSpec
		wantErr  bool
	}{
		{"r2://devstrap-test", hubSpec{scheme: "r2", bucket: "devstrap-test"}, false},
		{"s3://my-bucket", hubSpec{scheme: "s3", bucket: "my-bucket"}, false},
		{"r2://devstrap-test?endpoint=http://localhost:9000", hubSpec{scheme: "r2", bucket: "devstrap-test", endpoint: "http://localhost:9000"}, false},
		{"r2://devstrap-test?endpoint=http://localhost:9000&region=us-east-1", hubSpec{scheme: "r2", bucket: "devstrap-test", endpoint: "http://localhost:9000", region: "us-east-1"}, false},
		{"r2://devstrap-test?region=auto", hubSpec{scheme: "r2", bucket: "devstrap-test", region: "auto"}, false},
		{"r2://user:key@bucket", hubSpec{}, true}, // credentials must not ride the URI
		{"r2://", hubSpec{}, true},                // no bucket
		{"file:///tmp/x", hubSpec{}, true},        // wrong scheme
		{"", hubSpec{}, true},                     // empty
	}
	for _, c := range cases {
		got, err := parseHubURI(c.uri)
		switch {
		case c.wantErr && err == nil:
			t.Errorf("parseHubURI(%q) = nil error, want error", c.uri)
		case c.wantErr:
			// expected error; spec ignored
		case err != nil:
			t.Errorf("parseHubURI(%q) = %v, want nil", c.uri, err)
		case got != c.wantSpec:
			t.Errorf("parseHubURI(%q) = %+v, want %+v", c.uri, got, c.wantSpec)
		}
	}
}

// TestParseHubURINoCredentialLeak locks in the security fix (P5-HUB-01 review):
// a URI carrying credentials must be rejected AND the secret must not appear in
// the error diagnostic (parseHubURI uses url.URL.Redacted()).
func TestParseHubURINoCredentialLeak(t *testing.T) {
	const secret = "supersecrettoken"
	_, err := parseHubURI("r2://AKIAEXAMPLE:" + secret + "@my-bucket")
	if err == nil {
		t.Fatal("parseHubURI: expected error for credentials-in-URI, got nil")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("parseHubURI error leaks the secret: %q", err.Error())
	}
}

func TestHubConfigured(t *testing.T) {
	cases := []struct {
		name    string
		hubFile string
		setHub  string
		wantErr bool
	}{
		{"hub-file set", "/tmp/hub.json", "", false},
		{"r2 uri", "", "r2://devstrap-test", false},
		{"r2 uri with endpoint", "", "r2://devstrap-test?endpoint=http://localhost:9000", false},
		{"file uri", "", "file:/tmp/hub.json", false},
		{"bad r2 uri", "", "r2://", true},
		{"no hub", "", "", true},
		{"unrecognized scheme", "", "ftp://x", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			opts := &options{v: viper.New()}
			if c.setHub != "" {
				opts.v.Set("hub", c.setHub)
			}
			err := hubConfigured(opts, c.hubFile)
			if c.wantErr && err == nil {
				t.Errorf("hubConfigured(%s): want error, got nil", c.name)
			}
			if !c.wantErr && err != nil {
				t.Errorf("hubConfigured(%s): want nil, got %v", c.name, err)
			}
		})
	}
}

// stubOp installs a fake `op` binary on PATH (the P6-QUAL-04 PATH-shim
// pattern) whose behavior is the given shell script body. Returns the temp
// dir so tests can inspect files the shim writes.
func stubOp(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	shim := filepath.Join(dir, "op")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func newHubCredOptions(t *testing.T) *options {
	t.Helper()
	opts := &options{v: viper.New()}
	opts.v.Set("home", t.TempDir())
	return opts
}

// TestResolveHubS3CredentialsOpRef (P6-HUB-02): an op:// secret ref resolves
// through `op read --no-newline` instead of being signed as the literal AWS
// secret (the pre-fix failure mode was an opaque SignatureDoesNotMatch).
func TestResolveHubS3CredentialsOpRef(t *testing.T) {
	dir := stubOp(t, `printf '%s ' "$@" > "$(dirname "$0")/op-args"; printf 'resolved-op-secret'`)
	opts := newHubCredOptions(t)
	opts.v.Set("hub_s3_access_key_id", "AKIALITERAL")
	opts.v.Set("hub_s3_secret_access_key", "op://vault/item/secret-key")

	creds, err := resolveHubS3Credentials(context.Background(), opts, nil, "ws_test")
	if err != nil {
		t.Fatalf("resolveHubS3Credentials: %v", err)
	}
	if creds.accessKeyID != "AKIALITERAL" {
		t.Errorf("accessKeyID = %q, want AKIALITERAL", creds.accessKeyID)
	}
	if got := creds.secret.Reveal(); got != "resolved-op-secret" {
		t.Errorf("secret = %q, want resolved-op-secret", got)
	}
	if creds.source != "op" {
		t.Errorf("source = %q, want op", creds.source)
	}
	args, err := os.ReadFile(filepath.Join(dir, "op-args"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "read --no-newline op://vault/item/secret-key") {
		t.Errorf("op invoked with %q, want read --no-newline <ref>", string(args))
	}
	// The Secret wrapper must not leak the value through formatting.
	if s := fmt.Sprintf("%v %s", creds.secret, creds.secret); strings.Contains(s, "resolved-op-secret") {
		t.Errorf("redact.Secret leaked the value: %q", s)
	}
}

// TestResolveHubS3CredentialsOpMissing: a clear, actionable error when an
// op:// ref is configured but the 1Password CLI is not installed.
func TestResolveHubS3CredentialsOpMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no op anywhere
	opts := newHubCredOptions(t)
	opts.v.Set("hub_s3_access_key_id", "AKIALITERAL")
	opts.v.Set("hub_s3_secret_access_key", "op://vault/item/secret-key")
	_, err := resolveHubS3Credentials(context.Background(), opts, nil, "ws_test")
	if err == nil || !strings.Contains(err.Error(), "1Password CLI") {
		t.Fatalf("err = %v, want 1Password CLI hint", err)
	}
}

// TestResolveHubS3CredentialsStoredPair (P6-HUB-02): with nothing in
// env/config, the pair stored by `hub login` is used — here via the 0600 file
// fallback (DEVSTRAP_NO_KEYCHAIN=1, the headless/CI custody path).
func TestResolveHubS3CredentialsStoredPair(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	opts := newHubCredOptions(t)
	keys := devicekeys.NewHybridStore(opts.paths().KeyDir(), platform.Detect().Keychain)
	location, err := keys.StoreHubS3Credentials(context.Background(), "ws_test", devicekeys.HubS3Credentials{
		AccessKeyID: "AKIASTORED", SecretAccessKey: "stored-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if location != "file" {
		t.Fatalf("location = %q, want file (DEVSTRAP_NO_KEYCHAIN)", location)
	}
	creds, err := resolveHubS3Credentials(context.Background(), opts, nil, "ws_test")
	if err != nil {
		t.Fatalf("resolveHubS3Credentials: %v", err)
	}
	if creds.accessKeyID != "AKIASTORED" || creds.secret.Reveal() != "stored-secret" || creds.source != "keychain" {
		t.Errorf("creds = {%s, source=%s}, want stored pair with source=keychain", creds.accessKeyID, creds.source)
	}
}

// TestResolveHubS3CredentialsEnvOverridesStored: explicit env/config values
// win over a stored pair (12-factor/CI override).
func TestResolveHubS3CredentialsEnvOverridesStored(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	opts := newHubCredOptions(t)
	keys := devicekeys.NewHybridStore(opts.paths().KeyDir(), platform.Detect().Keychain)
	if _, err := keys.StoreHubS3Credentials(context.Background(), "ws_test", devicekeys.HubS3Credentials{
		AccessKeyID: "AKIASTORED", SecretAccessKey: "stored-secret",
	}); err != nil {
		t.Fatal(err)
	}
	opts.v.Set("hub_s3_access_key_id", "AKIAENV")
	opts.v.Set("hub_s3_secret_access_key", "env-secret")
	creds, err := resolveHubS3Credentials(context.Background(), opts, nil, "ws_test")
	if err != nil {
		t.Fatal(err)
	}
	if creds.accessKeyID != "AKIAENV" || creds.secret.Reveal() != "env-secret" || creds.source != "env/config" {
		t.Errorf("creds = {%s, source=%s}, want explicit env/config pair", creds.accessKeyID, creds.source)
	}
}

// TestResolveHubS3CredentialsNothingConfigured: the failure names both
// remedies (env vars / hub login) instead of an opaque SDK error downstream.
func TestResolveHubS3CredentialsNothingConfigured(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	opts := newHubCredOptions(t)
	_, err := resolveHubS3Credentials(context.Background(), opts, nil, "ws_test")
	if err == nil || !strings.Contains(err.Error(), "devstrap hub login") || !strings.Contains(err.Error(), "DEVSTRAP_HUB_S3_ACCESS_KEY_ID") {
		t.Fatalf("err = %v, want both remedies named", err)
	}
}

// TestHubLoginLogoutRoundTrip (P6-HUB-02): `hub login` stores the pair (secret
// via piped stdin, never argv), sync-time resolution finds it, and `hub
// logout` removes it.
func TestHubLoginLogoutRoundTrip(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init: %v (%s)", err, stderr)
	}

	var stdout, stderr bytes.Buffer
	cmd := NewRootCommand(&stdout, &stderr)
	cmd.SetIn(strings.NewReader("login-secret-value\n"))
	cmd.SetArgs([]string{"--home", home, "--root", root, "hub", "login", "--access-key-id", "AKIALOGIN"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("hub login: %v (%s)", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "file store") {
		t.Errorf("login output = %q, want file-store location report", stdout.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), "login-secret-value") {
		t.Fatalf("login output leaked the secret")
	}

	opts := &options{v: viper.New()}
	opts.v.Set("home", home)
	// Workspace id from the initialized store.
	st, err := opts.openState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(st)
	ws, err := st.WorkspaceID(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	creds, err := resolveHubS3Credentials(context.Background(), opts, st, ws)
	if err != nil {
		t.Fatalf("resolve after login: %v", err)
	}
	if creds.accessKeyID != "AKIALOGIN" || creds.secret.Reveal() != "login-secret-value" {
		t.Errorf("resolved creds = %q/<secret>, want the login pair", creds.accessKeyID)
	}

	stdout.Reset()
	cmd = NewRootCommand(&stdout, &stderr)
	cmd.SetArgs([]string{"--home", home, "--root", root, "hub", "logout"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("hub logout: %v (%s)", err, stderr.String())
	}
	if _, err := resolveHubS3Credentials(context.Background(), opts, st, ws); err == nil {
		t.Fatal("resolve after logout: want error, got credentials")
	}
}

// TestHubLoginJSON pins the P5-CLI-01 part B --json shape for hub login
// (workspace id + custody backend name; never the secret).
func TestHubLoginJSON(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init: %v (%s)", err, stderr)
	}

	var stdout, stderr bytes.Buffer
	cmd := NewRootCommand(&stdout, &stderr)
	cmd.SetIn(strings.NewReader("login-secret-json\n"))
	cmd.SetArgs([]string{"--home", home, "--root", root, "--json", "hub", "login", "--access-key-id", "AKIAJSON"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("hub login --json: %v (%s)", err, stderr.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), "login-secret-json") {
		t.Fatalf("login --json leaked the secret: %s / %s", stdout.String(), stderr.String())
	}
	var got hubLoginResult
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("hub login --json is not a hubLoginResult: %v\n%s", err, stdout.String())
	}
	if got.WorkspaceID == "" || !strings.HasPrefix(got.WorkspaceID, "ws_") {
		t.Errorf("workspace_id = %q, want ws_…", got.WorkspaceID)
	}
	if got.CredentialStore != "file" {
		t.Errorf("credential_store = %q, want file (DEVSTRAP_NO_KEYCHAIN=1)", got.CredentialStore)
	}
}

// TestHubLogoutJSON pins the P5-CLI-01 part B --json shape for hub logout.
func TestHubLogoutJSON(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init: %v (%s)", err, stderr)
	}

	// Absent credentials: removed=false.
	stdout, stderrOut, err := executeForTest("--home", home, "--root", root, "--json", "hub", "logout")
	if err != nil {
		t.Fatalf("hub logout --json (absent): %v (%s)", err, stderrOut)
	}
	var got hubLogoutResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("hub logout --json is not a hubLogoutResult: %v\n%s", err, stdout)
	}
	if got.Removed {
		t.Error("removed = true when no credentials stored, want false")
	}
	if got.WorkspaceID == "" {
		t.Error("workspace_id is empty")
	}

	// Store then logout: removed=true.
	var out, errb bytes.Buffer
	cmd := NewRootCommand(&out, &errb)
	cmd.SetIn(strings.NewReader("logout-json-secret\n"))
	cmd.SetArgs([]string{"--home", home, "--root", root, "hub", "login", "--access-key-id", "AKIALOGOUT"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("hub login setup: %v (%s)", err, errb.String())
	}
	stdout, stderrOut, err = executeForTest("--home", home, "--root", root, "--json", "hub", "logout")
	if err != nil {
		t.Fatalf("hub logout --json (present): %v (%s)", err, stderrOut)
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("hub logout --json after login: %v\n%s", err, stdout)
	}
	if !got.Removed {
		t.Error("removed = false after login, want true")
	}
}

// TestHubGCJSON pins the P5-CLI-01 part B --json shape for hub gc.
func TestHubGCJSON(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	hubPath := filepath.Join(t.TempDir(), "hub.json")
	if err := os.WriteFile(hubPath, []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init: %v (%s)", err, stderr)
	}

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "--json", "hub", "gc", "--hub-file", hubPath, "--dry-run")
	if err != nil {
		t.Fatalf("hub gc --json: %v (%s)", err, stderr)
	}
	var got hubGCResult
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("hub gc --json is not a hubGCResult: %v\n%s", err, stdout)
	}
	if !got.DryRun {
		t.Error("dry_run = false, want true")
	}
	// An empty hub with a freshly-initialized store has nothing to prune or
	// remove — assert the real expected zero values, not just non-negative
	// (review finding, PR #203).
	if got.PrunedSnapshots != 0 {
		t.Errorf("pruned_snapshots = %d, want 0 (empty hub/fresh store)", got.PrunedSnapshots)
	}
	if got.RemovedBlobs != 0 {
		t.Errorf("removed_blobs = %d, want 0 (empty hub/fresh store)", got.RemovedBlobs)
	}
	if strings.Contains(stdout, "hub gc:") {
		t.Fatalf("hub gc --json leaked human summary: %s", stdout)
	}
}

// TestHubLoginRefusesOpRef: op:// refs belong in env/config (resolved at sync
// time), not frozen into the custody store as literals.
func TestHubLoginRefusesOpRef(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init"); err != nil {
		t.Fatalf("init: %v (%s)", err, stderr)
	}
	var stdout, stderr bytes.Buffer
	cmd := NewRootCommand(&stdout, &stderr)
	cmd.SetIn(strings.NewReader("op://vault/item/key\n"))
	cmd.SetArgs([]string{"--home", home, "--root", root, "hub", "login", "--access-key-id", "AKIALOGIN"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "op://") {
		t.Fatalf("hub login with op:// secret: err = %v, want refusal", err)
	}
}

// TestResolveHubS3CredentialsOpSecretWithStoredKeyID (post-#45 review Major,
// gpt-5.5): an op:// secret must NOT short-circuit the keychain fill for the
// access key id — the "hub login stored key id + rotated op:// secret"
// combination the resolver's contract promises.
func TestResolveHubS3CredentialsOpSecretWithStoredKeyID(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	stubOp(t, `printf 'rotated-op-secret'`)
	opts := newHubCredOptions(t)
	keys := devicekeys.NewHybridStore(opts.paths().KeyDir(), platform.Detect().Keychain)
	if _, err := keys.StoreHubS3Credentials(context.Background(), "ws_test", devicekeys.HubS3Credentials{
		AccessKeyID: "AKIASTORED", SecretAccessKey: "old-stored-secret",
	}); err != nil {
		t.Fatal(err)
	}
	opts.v.Set("hub_s3_secret_access_key", "op://vault/item/rotated")

	creds, err := resolveHubS3Credentials(context.Background(), opts, nil, "ws_test")
	if err != nil {
		t.Fatalf("resolveHubS3Credentials: %v", err)
	}
	if creds.accessKeyID != "AKIASTORED" {
		t.Errorf("accessKeyID = %q, want the stored AKIASTORED (op:// secret must not skip the keychain fill)", creds.accessKeyID)
	}
	if got := creds.secret.Reveal(); got != "rotated-op-secret" {
		t.Errorf("secret = %q, want the op-resolved value (explicit env wins over stored)", got)
	}
}

// TestResolveHubS3CredentialsOpSecretMissingKeyID: op:// secret with no access
// key id anywhere must fail with the crafted two-remedy error, not return
// half-empty credentials.
func TestResolveHubS3CredentialsOpSecretMissingKeyID(t *testing.T) {
	t.Setenv(platform.NoKeychainEnv, "1")
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	stubOp(t, `printf 'resolved-op-secret'`)
	opts := newHubCredOptions(t)
	opts.v.Set("hub_s3_secret_access_key", "op://vault/item/secret-key")

	_, err := resolveHubS3Credentials(context.Background(), opts, nil, "ws_test")
	if err == nil || !strings.Contains(err.Error(), "DEVSTRAP_HUB_S3_ACCESS_KEY_ID") || !strings.Contains(err.Error(), "devstrap hub login") {
		t.Fatalf("err = %v, want the two-remedy no-credentials error", err)
	}
}

// TestHubS3CredsNeverFormatsSecret (post-#45 review, gpt-5.5): fmt cannot
// dispatch a Stringer on an UNEXPORTED struct field, so without hubS3Creds's
// own String/GoString/LogValue a %+v would dump the raw secret. Pin every
// common formatting path.
func TestHubS3CredsNeverFormatsSecret(t *testing.T) {
	creds := hubS3Creds{accessKeyID: "AKIAFMT", secret: redact.New("super-secret-value"), source: "env/config"}
	for _, s := range []string{
		fmt.Sprint(creds),
		fmt.Sprintf("%v", creds),
		fmt.Sprintf("%+v", creds),
		fmt.Sprintf("%#v", creds),
		creds.String(),
	} {
		if strings.Contains(s, "super-secret-value") {
			t.Fatalf("formatting leaked the secret: %q", s)
		}
		if !strings.Contains(s, "AKIAFMT") {
			t.Fatalf("formatting lost the non-secret fields: %q", s)
		}
	}
}

// TestResolveOpRefTimesOut (review nit): a wedged `op` prompt cannot hold a
// sync cycle open — resolveOpRef bounds the subprocess with opReadTimeout.
func TestResolveOpRefTimesOut(t *testing.T) {
	stubOp(t, `exec sleep 5`)
	old := opReadTimeout
	opReadTimeout = 200 * time.Millisecond
	t.Cleanup(func() { opReadTimeout = old })
	start := time.Now()
	_, err := resolveOpRef(context.Background(), "op://vault/item/slow")
	if err == nil {
		t.Fatal("resolveOpRef with a wedged op: want timeout error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("resolveOpRef took %v; the timeout did not bound the subprocess", elapsed)
	}
}
