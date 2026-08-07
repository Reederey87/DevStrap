package onepassword

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubOp installs a fake `op` binary on PATH for the duration of the test
// (the FORGE-05/P6-QUAL-04 PATH-shim pattern used throughout internal/cli).
func stubOp(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "op"), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func TestLookPathMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	err := LookPath()
	if err == nil {
		t.Fatal("LookPath err = nil, want an error when op is not on PATH")
	}
	if !strings.Contains(err.Error(), "1Password CLI") {
		t.Fatalf("LookPath err = %v, want it to mention the 1Password CLI", err)
	}
}

func TestLookPathPresent(t *testing.T) {
	stubOp(t, "exit 0")
	if err := LookPath(); err != nil {
		t.Fatalf("LookPath err = %v, want nil once op is on PATH", err)
	}
}

func TestListItems(t *testing.T) {
	stubOp(t, `
printf '%s\n' "$@" > "$(dirname "$0")/args"
cat <<'EOF'
[{"id":"item1","title":"Acme API","vault":{"id":"v1","name":"Engineering"}}]
EOF
`)
	items, err := ListItems(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != "item1" || items[0].Title != "Acme API" || items[0].Vault.Name != "Engineering" {
		t.Fatalf("items = %#v", items)
	}
}

func TestListItemsScopesToVault(t *testing.T) {
	dir := stubOp(t, `
printf '%s\n' "$@" > "$(dirname "$0")/args"
echo '[]'
`)
	if _, err := ListItems(context.Background(), "Engineering"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "--vault\nEngineering") {
		t.Fatalf("args = %q, want a --vault Engineering flag", raw)
	}
}

// TestListFieldsNeverExposesValue asserts that even if the (stubbed) CLI's
// JSON includes a field's value, ListFields's Field type has no member to
// decode it into -- the value is structurally unreachable, not just
// unprinted.
func TestListFieldsNeverExposesValue(t *testing.T) {
	stubOp(t, `
cat <<'EOF'
{"fields":[{"id":"password","label":"password","type":"CONCEALED","value":"should-be-unreachable"}]}
EOF
`)
	fields, err := ListFields(context.Background(), "item1")
	if err != nil {
		t.Fatal(err)
	}
	if len(fields) != 1 || fields[0].Label != "password" {
		t.Fatalf("fields = %#v", fields)
	}
	// %+v over the Field struct must never be able to print the value, since
	// there is no field to hold it.
	if s := fmt.Sprintf("%+v", fields[0]); strings.Contains(s, "should-be-unreachable") {
		t.Fatalf("Field formatting leaked a value: %q", s)
	}
}

func TestListItemsNeverPassesReveal(t *testing.T) {
	dir := stubOp(t, `
printf '%s\n' "$@" > "$(dirname "$0")/args"
echo '[]'
`)
	if _, err := ListItems(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "args"))
	if strings.Contains(string(raw), "--reveal") {
		t.Fatalf("ListItems invoked op with --reveal: %q", raw)
	}
}

// TestSetFieldUsesTemplateNeverBareAssignment is the core security-surface
// test for the write-back path: the value must travel only through a
// --template=<file> JSON file, never as a bare `field=value` CLI argument
// (1Password's own CLI best-practices guidance: an inline assignment is
// visible in shell history and to other local processes).
func TestSetFieldUsesTemplateNeverBareAssignment(t *testing.T) {
	dir := stubOp(t, `
printf '%s\n' "$@" > "$(dirname "$0")/args"
for arg do
  case "$arg" in
    --template=*)
      cp "${arg#--template=}" "$(dirname "$0")/template-copy.json"
      ;;
  esac
done
echo '{}'
`)
	const secret = "correct-horse-battery-staple"
	ref, err := SetField(context.Background(), "Engineering", "Acme API", "api_key", secret)
	if err != nil {
		t.Fatal(err)
	}
	if ref != "op://Engineering/Acme API/api_key" {
		t.Fatalf("ref = %q", ref)
	}

	rawArgs, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil {
		t.Fatal(err)
	}
	argsText := string(rawArgs)
	if strings.Contains(argsText, secret) {
		t.Fatalf("op invocation args leaked the secret value: %q", argsText)
	}
	for _, line := range strings.Split(argsText, "\n") {
		if strings.Contains(line, "=") && !strings.HasPrefix(line, "--template=") {
			t.Fatalf("op invocation contained a non-template assignment-shaped argument: %q", line)
		}
	}
	wantArgs := []string{"item", "edit", "Acme API", "--vault", "Engineering"}
	for _, want := range wantArgs {
		if !strings.Contains(argsText, want) {
			t.Fatalf("op invocation args = %q, want it to contain %q", argsText, want)
		}
	}

	tmplRaw, err := os.ReadFile(filepath.Join(dir, "template-copy.json"))
	if err != nil {
		t.Fatalf("op item edit was not invoked with a readable --template= file: %v", err)
	}
	if !strings.Contains(string(tmplRaw), secret) {
		t.Fatalf("captured template = %q, want it to contain the secret value", tmplRaw)
	}
}

// TestSetFieldRemovesTemplateAfterReturn: the private temp dir (and the
// template file inside it) must not survive past SetField's return, success
// or error.
func TestSetFieldRemovesTemplateAfterReturn(t *testing.T) {
	stubOp(t, `
for arg do
  case "$arg" in
    --template=*)
      path="${arg#--template=}"
      dirname "$path" > "$(dirname "$0")/captured-dir"
      ;;
  esac
done
echo '{}'
`)
	if _, err := SetField(context.Background(), "vault", "item", "field", "value"); err != nil {
		t.Fatal(err)
	}
	// Find the stub dir via PATH (set by stubOp) to read back the captured path.
	pathDirs := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))
	if len(pathDirs) == 0 {
		t.Fatal("PATH is empty")
	}
	capturedPathFile := filepath.Join(pathDirs[0], "captured-dir")
	raw, err := os.ReadFile(capturedPathFile)
	if err != nil {
		t.Fatalf("op item edit was not invoked: %v", err)
	}
	capturedDir := strings.TrimSpace(string(raw))
	if capturedDir == "" {
		t.Fatal("captured template dir path is empty")
	}
	if _, statErr := os.Stat(capturedDir); !os.IsNotExist(statErr) {
		t.Fatalf("template dir %s still exists after SetField returned (stat err = %v)", capturedDir, statErr)
	}
}

func TestSetFieldRemovesTemplateOnOpFailure(t *testing.T) {
	stubOp(t, `
for arg do
  case "$arg" in
    --template=*)
      path="${arg#--template=}"
      dirname "$path" > "$(dirname "$0")/captured-dir"
      ;;
  esac
done
echo "forced failure" >&2
exit 1
`)
	_, err := SetField(context.Background(), "vault", "item", "field", "value")
	if err == nil {
		t.Fatal("SetField err = nil, want an error when op fails")
	}
	pathDirs := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))
	capturedPathFile := filepath.Join(pathDirs[0], "captured-dir")
	raw, readErr := os.ReadFile(capturedPathFile)
	if readErr != nil {
		t.Fatalf("op item edit was not invoked: %v", readErr)
	}
	capturedDir := strings.TrimSpace(string(raw))
	if _, statErr := os.Stat(capturedDir); !os.IsNotExist(statErr) {
		t.Fatalf("template dir %s still exists after a failed SetField (stat err = %v)", capturedDir, statErr)
	}
}

func TestSetFieldRequiresAllThreeParts(t *testing.T) {
	stubOp(t, "echo '{}'")
	if _, err := SetField(context.Background(), "", "item", "field", "value"); err == nil {
		t.Fatal("SetField err = nil, want an error for an empty vault")
	}
	if _, err := SetField(context.Background(), "vault", "", "field", "value"); err == nil {
		t.Fatal("SetField err = nil, want an error for an empty item")
	}
	if _, err := SetField(context.Background(), "vault", "item", "", "value"); err == nil {
		t.Fatal("SetField err = nil, want an error for an empty field")
	}
}
