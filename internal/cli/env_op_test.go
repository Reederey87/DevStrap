package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/state"
)

// opSecretValueMarker is a fixture value that must never reach devstrap's own
// stdout/stderr or a bare `op` subprocess argument.
const opSecretValueMarker = "super-secret-1password-value-xyz"

// opFieldValueMarker simulates a value 1Password's own `op item get
// --format=json` might include for a concealed field even without --reveal;
// `env op list` must never print it regardless of what the (stubbed) CLI
// returns, because the Field type it decodes into has no value member.
const opFieldValueMarker = "never-should-be-printed-field-value"

// opBrowseStubScript builds a PATH-shimmed `op` stub (the FORGE-05/P6-QUAL-04
// technique, see stubForgeCLI/stubOp) that answers `item list`/`item
// get`/`item edit`. Every invocation's full argv is appended to argsFile (one
// line per arg, one blank-line-separated call per invocation via a leading
// marker), so a test can assert exactly what devstrap ran `op` with. An `item
// edit` call additionally copies its `--template=` file to templateCopyPath
// *while the file still exists* (devstrap deletes it immediately after the
// subprocess returns), so the test can inspect what actually went into the
// template without racing the cleanup.
func opBrowseStubScript(argsFile, templateCopyPath string) string {
	return fmt.Sprintf(`
{
  echo '--- op call ---'
  printf '%%s\n' "$@"
} >> %[1]q
if [ "$1" = "item" ] && [ "$2" = "list" ]; then
  cat <<'DEVSTRAP_ITEM_LIST_EOF'
[{"id":"item1","title":"Acme API","vault":{"id":"v1","name":"Engineering"}}]
DEVSTRAP_ITEM_LIST_EOF
  exit 0
fi
if [ "$1" = "item" ] && [ "$2" = "get" ]; then
  cat <<DEVSTRAP_ITEM_GET_EOF
{"fields":[{"id":"username","label":"username","type":"STRING"},{"id":"password","label":"password","type":"CONCEALED","value":"%[3]s"}]}
DEVSTRAP_ITEM_GET_EOF
  exit 0
fi
if [ "$1" = "item" ] && [ "$2" = "edit" ]; then
  for arg do
    case "$arg" in
      --template=*)
        cp "${arg#--template=}" %[2]q
        ;;
    esac
  done
  echo '{"id":"item1"}'
  exit 0
fi
echo "unexpected op invocation: $*" >&2
exit 1
`, argsFile, templateCopyPath, opFieldValueMarker)
}

func mustInitWorkspace(t *testing.T, home, root string) {
	t.Helper()
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init", "--workspace-name", "personal"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
}

func upsertOpTestProject(t *testing.T, home string) {
	t.Helper()
	ctx := context.Background()
	store, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(store)
	if _, err := store.UpsertProject(ctx, state.UpsertProjectParams{
		Path: "work/proj", Type: "git_repo", RemoteKey: "github.com/acme/proj", RemoteURL: "https://github.com/acme/proj",
	}); err != nil {
		t.Fatal(err)
	}
}

// TestEnvOpListPrintsRefsNeverValues (W12-03): `env op list` must print
// copyable op://vault/item/field references and must never print a field's
// value, even when the (stubbed) `op item get --format=json` output includes
// one, since the Field type it decodes into simply has no value member.
func TestEnvOpListPrintsRefsNeverValues(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	mustInitWorkspace(t, home, root)

	argsFile := filepath.Join(t.TempDir(), "op-args.txt")
	tmplCopy := filepath.Join(t.TempDir(), "template-copy.json")
	stubOp(t, opBrowseStubScript(argsFile, tmplCopy))

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "env", "op", "list")
	if err != nil {
		t.Fatalf("env op list stderr = %q err = %v", stderr, err)
	}
	if strings.Contains(stdout, opFieldValueMarker) || strings.Contains(stderr, opFieldValueMarker) {
		t.Fatalf("env op list leaked a field value into output: stdout=%q stderr=%q", stdout, stderr)
	}
	want := "op://Engineering/Acme API/username"
	if !strings.Contains(stdout, want) {
		t.Fatalf("env op list stdout = %q, want it to contain %q", stdout, want)
	}
	wantPassword := "op://Engineering/Acme API/password"
	if !strings.Contains(stdout, wantPassword) {
		t.Fatalf("env op list stdout = %q, want it to contain %q (label only, no value)", stdout, wantPassword)
	}

	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "--reveal") {
		t.Fatalf("env op list invoked op with --reveal: %s", raw)
	}
}

// TestEnvOpListMissingCLI (W12-03): a clear, actionable error naming the
// missing 1Password CLI, gating the whole `env op` group.
func TestEnvOpListMissingCLI(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	mustInitWorkspace(t, home, root)
	t.Setenv("PATH", t.TempDir()) // no op anywhere

	_, stderr, err := executeForTest("--home", home, "--root", root, "env", "op", "list")
	if err == nil {
		t.Fatal("env op list err = nil, want an error when op is missing")
	}
	if !strings.Contains(stderr, "1Password CLI") {
		t.Fatalf("env op list stderr = %q, want it to mention the 1Password CLI", stderr)
	}
}

// TestEnvOpSetMissingCLI mirrors TestEnvOpListMissingCLI for `env op set`.
func TestEnvOpSetMissingCLI(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	mustInitWorkspace(t, home, root)
	upsertOpTestProject(t, home)
	t.Setenv("PATH", t.TempDir()) // no op anywhere

	_, stderr, err := executeForTest("--home", home, "--root", root, "env", "op", "set", "work/proj", "API_KEY", "op://vault/item/field")
	if err == nil {
		t.Fatal("env op set err = nil, want an error when op is missing")
	}
	if !strings.Contains(stderr, "1Password CLI") {
		t.Fatalf("env op set stderr = %q, want it to mention the 1Password CLI", stderr)
	}
}

// TestEnvOpSetFromOpRefNeverInvokesOp (W12-03): when the value already looks
// like an op:// reference, `env op set` must bind it directly — the same
// write path `env bind` uses — and must never shell out to `op` at all (no
// item edit, no item get, no item list).
func TestEnvOpSetFromOpRefNeverInvokesOp(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	mustInitWorkspace(t, home, root)
	upsertOpTestProject(t, home)

	argsFile := filepath.Join(t.TempDir(), "op-args.txt")
	tmplCopy := filepath.Join(t.TempDir(), "template-copy.json")
	stubOp(t, opBrowseStubScript(argsFile, tmplCopy))

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "env", "op", "set", "work/proj", "API_KEY", "op://vault/item/field")
	if err != nil {
		t.Fatalf("env op set stderr = %q err = %v", stderr, err)
	}
	if !strings.Contains(stdout, "op://vault/item/field") {
		t.Fatalf("env op set stdout = %q, want it to echo the bound ref", stdout)
	}
	if _, err := os.Stat(argsFile); err == nil {
		t.Fatalf("env op set with an op:// value invoked the op CLI (argsFile exists): binding an existing ref must never shell out")
	}

	ctx := context.Background()
	store, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(store)
	project, err := store.ProjectByPath(ctx, "work/proj")
	if err != nil {
		t.Fatal(err)
	}
	profile, bindings, err := store.EnvProfileForProject(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Provider != "1password" || len(bindings) != 1 || bindings[0].VarName != "API_KEY" || bindings[0].ProviderRef != "op://vault/item/field" {
		t.Fatalf("profile=%#v bindings=%#v", profile, bindings)
	}
}

// TestEnvOpSetFromPlaintextUsesTemplateFile is the core security-surface test
// (W12-03): given a plaintext value, `env op set` must write it into
// 1Password only through a `--template=<file>` JSON template — never as a
// bare `field=value` CLI argument, per 1Password's own CLI best-practices
// guidance (an inline assignment is visible in shell history/process
// listings). It also asserts the template file is gone once the command
// returns.
func TestEnvOpSetFromPlaintextUsesTemplateFile(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	mustInitWorkspace(t, home, root)
	upsertOpTestProject(t, home)

	argsFile := filepath.Join(t.TempDir(), "op-args.txt")
	tmplCopy := filepath.Join(t.TempDir(), "template-copy.json")
	stubOp(t, opBrowseStubScript(argsFile, tmplCopy))

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "env", "op", "set",
		"work/proj", "API_KEY", opSecretValueMarker, "--vault", "Engineering", "--item", "Acme API", "--field", "api_key")
	if err != nil {
		t.Fatalf("env op set stderr = %q err = %v", stderr, err)
	}
	if strings.Contains(stdout, opSecretValueMarker) || strings.Contains(stderr, opSecretValueMarker) {
		t.Fatalf("env op set leaked the plaintext value into output: stdout=%q stderr=%q", stdout, stderr)
	}
	wantRef := "op://Engineering/Acme API/api_key"
	if !strings.Contains(stdout, wantRef) {
		t.Fatalf("env op set stdout = %q, want it to contain the resulting ref %q", stdout, wantRef)
	}
	if !strings.Contains(stderr, wantRef) || !strings.Contains(stderr, "Writing") {
		t.Fatalf("env op set stderr = %q, want a transparency line naming the write target %q before it happens (Fable UX review)", stderr, wantRef)
	}

	// The recorded op invocation must use --template=, and no single argument
	// anywhere may be (or contain) a bare `field=value` assignment carrying
	// the plaintext value.
	rawArgs, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	argsText := string(rawArgs)
	if !strings.Contains(argsText, "--template=") {
		t.Fatalf("op invocation args = %q, want a --template= argument", argsText)
	}
	if strings.Contains(argsText, opSecretValueMarker) {
		t.Fatalf("op invocation args leaked the plaintext value: %q", argsText)
	}
	for _, line := range strings.Split(argsText, "\n") {
		if strings.Contains(line, "=") && strings.Contains(line, opSecretValueMarker) {
			t.Fatalf("op invocation contained an inline assignment-style argument: %q", line)
		}
	}

	// The template file the stub captured (before devstrap deleted it) must
	// be the one place the plaintext value actually appears.
	tmplRaw, err := os.ReadFile(tmplCopy)
	if err != nil {
		t.Fatalf("op item edit was never invoked with a readable --template= file: %v", err)
	}
	if !strings.Contains(string(tmplRaw), opSecretValueMarker) {
		t.Fatalf("captured op edit template = %q, want it to contain the plaintext value", tmplRaw)
	}

	// The original template file (and its private temp dir) must no longer
	// exist once the command has returned.
	for _, line := range strings.Split(argsText, "\n") {
		if path, ok := strings.CutPrefix(strings.TrimSpace(line), "--template="); ok {
			if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
				t.Fatalf("template file %s still exists after env op set returned (stat err = %v)", path, statErr)
			}
			if _, statErr := os.Stat(filepath.Dir(path)); !os.IsNotExist(statErr) {
				t.Fatalf("template dir %s still exists after env op set returned (stat err = %v)", filepath.Dir(path), statErr)
			}
		}
	}

	ctx := context.Background()
	store, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(store)
	project, err := store.ProjectByPath(ctx, "work/proj")
	if err != nil {
		t.Fatal(err)
	}
	_, bindings, err := store.EnvProfileForProject(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 || bindings[0].ProviderRef != wantRef {
		t.Fatalf("bindings = %#v, want a single API_KEY -> %s binding", bindings, wantRef)
	}
}

// TestEnvOpSetReadsPlaintextFromStdin: passing "-" reads the value from
// stdin instead of argv, so the value need not appear in devstrap's own
// shell history either.
func TestEnvOpSetReadsPlaintextFromStdin(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	mustInitWorkspace(t, home, root)
	upsertOpTestProject(t, home)

	argsFile := filepath.Join(t.TempDir(), "op-args.txt")
	tmplCopy := filepath.Join(t.TempDir(), "template-copy.json")
	stubOp(t, opBrowseStubScript(argsFile, tmplCopy))

	stdout, stderr, err := executeForTestWithStdin(strings.NewReader(opSecretValueMarker+"\n"),
		"--home", home, "--root", root, "env", "op", "set",
		"work/proj", "API_KEY", "-", "--vault", "Engineering", "--item", "Acme API")
	if err != nil {
		t.Fatalf("env op set stderr = %q err = %v", stderr, err)
	}
	if strings.Contains(stdout, opSecretValueMarker) || strings.Contains(stderr, opSecretValueMarker) {
		t.Fatalf("env op set leaked the stdin value into output: stdout=%q stderr=%q", stdout, stderr)
	}
	tmplRaw, err := os.ReadFile(tmplCopy)
	if err != nil {
		t.Fatalf("op item edit was never invoked: %v", err)
	}
	if !strings.Contains(string(tmplRaw), opSecretValueMarker) {
		t.Fatalf("captured op edit template = %q, want it to contain the stdin value", tmplRaw)
	}
}

// TestEnvOpSetPlaintextRequiresVaultAndItemForNewKey: a plaintext value for a
// key with no existing binding cannot be written anywhere without knowing the
// target vault/item, and must fail before ever invoking op.
func TestEnvOpSetPlaintextRequiresVaultAndItemForNewKey(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	mustInitWorkspace(t, home, root)
	upsertOpTestProject(t, home)

	argsFile := filepath.Join(t.TempDir(), "op-args.txt")
	tmplCopy := filepath.Join(t.TempDir(), "template-copy.json")
	stubOp(t, opBrowseStubScript(argsFile, tmplCopy))

	_, stderr, err := executeForTest("--home", home, "--root", root, "env", "op", "set", "work/proj", "API_KEY", opSecretValueMarker)
	if err == nil {
		t.Fatal("env op set err = nil, want an error for a brand new key with no --vault/--item")
	}
	if !strings.Contains(stderr, "--vault") || !strings.Contains(stderr, "--item") {
		t.Fatalf("env op set stderr = %q, want it to mention --vault and --item", stderr)
	}
	if _, statErr := os.Stat(argsFile); statErr == nil {
		t.Fatal("env op set invoked op despite refusing for a missing --vault/--item")
	}
}

// TestEnvOpSetPlaintextDefaultsToExistingBinding: rotating an already-bound
// key's value needs no --vault/--item/--field -- they default to the
// existing op:// binding's vault/item/field.
func TestEnvOpSetPlaintextDefaultsToExistingBinding(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	mustInitWorkspace(t, home, root)
	upsertOpTestProject(t, home)

	argsFile := filepath.Join(t.TempDir(), "op-args.txt")
	tmplCopy := filepath.Join(t.TempDir(), "template-copy.json")
	stubOp(t, opBrowseStubScript(argsFile, tmplCopy))

	if _, stderr, err := executeForTest("--home", home, "--root", root, "env", "op", "set", "work/proj", "API_KEY", "op://Engineering/Acme API/api_key"); err != nil {
		t.Fatalf("seed bind stderr = %q err = %v", stderr, err)
	}
	if err := os.Remove(argsFile); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "env", "op", "set", "work/proj", "API_KEY", opSecretValueMarker)
	if err != nil {
		t.Fatalf("env op set stderr = %q err = %v", stderr, err)
	}
	wantRef := "op://Engineering/Acme API/api_key"
	if !strings.Contains(stdout, wantRef) {
		t.Fatalf("env op set stdout = %q, want the same ref %q reused from the existing binding", stdout, wantRef)
	}
}

// TestEnvOpSetMergesWithExistingRefs (W12-03): UpsertEnvProfileTx replaces a
// project's whole provider-ref map on every write, so `env op set` must merge
// the key it is setting into the existing bindings rather than clobbering
// them -- binding a second key via `env op set` must leave a first key bound
// by `env bind` intact.
func TestEnvOpSetMergesWithExistingRefs(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	mustInitWorkspace(t, home, root)
	upsertOpTestProject(t, home)

	projDir := filepath.Join(root, "work", "proj")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}
	refsPath := filepath.Join(projDir, ".env.refs")
	if err := os.WriteFile(refsPath, []byte("DB_URL=op://vault/item/db\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := executeForTest("--home", home, "--root", root, "env", "bind", "work/proj", ".env.refs", "--provider", "1password"); err != nil {
		t.Fatalf("env bind stderr = %q err = %v", stderr, err)
	}

	stubOp(t, opBrowseStubScript(filepath.Join(t.TempDir(), "op-args.txt"), filepath.Join(t.TempDir(), "template-copy.json")))
	if _, stderr, err := executeForTest("--home", home, "--root", root, "env", "op", "set", "work/proj", "API_KEY", "op://vault/item/api"); err != nil {
		t.Fatalf("env op set stderr = %q err = %v", stderr, err)
	}

	ctx := context.Background()
	store, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(store)
	project, err := store.ProjectByPath(ctx, "work/proj")
	if err != nil {
		t.Fatal(err)
	}
	_, bindings, err := store.EnvProfileForProject(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 2 {
		t.Fatalf("bindings = %#v, want 2 (DB_URL preserved, API_KEY added)", bindings)
	}
	byName := map[string]string{}
	for _, b := range bindings {
		byName[b.VarName] = b.ProviderRef
	}
	if byName["DB_URL"] != "op://vault/item/db" || byName["API_KEY"] != "op://vault/item/api" {
		t.Fatalf("bindings by name = %#v", byName)
	}
}

// TestEnvOpSetStdinValueOverLimitRefused (post-review hardening, W12-03): an
// oversized stdin value must be refused explicitly, not silently truncated
// and written as a partial value.
func TestEnvOpSetStdinValueOverLimitRefused(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	mustInitWorkspace(t, home, root)
	upsertOpTestProject(t, home)

	stubOp(t, opBrowseStubScript(filepath.Join(t.TempDir(), "op-args.txt"), filepath.Join(t.TempDir(), "template-copy.json")))

	oversized := strings.Repeat("x", maxStdinValueBytes+1)
	_, stderr, err := executeForTestWithStdin(strings.NewReader(oversized),
		"--home", home, "--root", root, "env", "op", "set",
		"work/proj", "API_KEY", "-", "--vault", "Engineering", "--item", "Acme API")
	if err == nil {
		t.Fatal("env op set err = nil, want an error for an oversized stdin value")
	}
	if !strings.Contains(stderr, "exceeds") {
		t.Fatalf("env op set stderr = %q, want it to mention the size limit", stderr)
	}
}

// TestProviderRefsForUpdateEmptyProfileIsNotFound (post-review hardening,
// W12-03): a project with no env profile at all must resolve through
// state.ErrEnvProfileNotFound (an empty map, no error) -- the deliberate
// carve-out in providerRefsForUpdate that distinguishes "nothing to merge
// into" from "the read failed, do not merge into an empty map."
func TestProviderRefsForUpdateEmptyProfileIsNotFound(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	mustInitWorkspace(t, home, root)
	upsertOpTestProject(t, home)

	ctx := context.Background()
	store, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeStore(store)
	project, err := store.ProjectByPath(ctx, "work/proj")
	if err != nil {
		t.Fatal(err)
	}

	_, _, profileErr := store.EnvProfileForProject(ctx, project.ID)
	if !strings.Contains(profileErr.Error(), "env profile not found") {
		t.Fatalf("precondition: EnvProfileForProject err = %v, want a not-found error", profileErr)
	}

	refs, err := providerRefsForUpdate(ctx, store, project)
	if err != nil {
		t.Fatalf("providerRefsForUpdate err = %v, want nil for a project with no profile yet", err)
	}
	if len(refs) != 0 {
		t.Fatalf("refs = %#v, want empty", refs)
	}
}

// TestEnvOpSetProfileReadFailurePropagates (post-review hardening, W12-03):
// providerRefsForUpdate must distinguish "no profile yet"
// (state.ErrEnvProfileNotFound) from a genuine read failure. A project that
// does not exist at all triggers ProjectByPath's own not-found error before
// providerRefsForUpdate is ever reached, which is the easiest way to exercise
// a real (non-ErrEnvProfileNotFound) failure through the command surface
// without a DB fault injector.
func TestEnvOpSetProfileReadFailurePropagates(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	mustInitWorkspace(t, home, root)
	stubOp(t, opBrowseStubScript(filepath.Join(t.TempDir(), "op-args.txt"), filepath.Join(t.TempDir(), "template-copy.json")))

	_, stderr, err := executeForTest("--home", home, "--root", root, "env", "op", "set", "work/does-not-exist", "API_KEY", "op://vault/item/field")
	if err == nil {
		t.Fatal("env op set err = nil, want an error for an unknown project")
	}
	if strings.Contains(stderr, "panic") {
		t.Fatalf("env op set stderr = %q, want a clean error, not a panic", stderr)
	}
}
