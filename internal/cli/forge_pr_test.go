package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/state"
	"github.com/spf13/viper"
)

// stubForgeCLI installs a fake forge CLI binary (gh/glab/tea) on PATH (the
// P6-QUAL-04 PATH-shim pattern, see stubSSH/stubOp). The stub records its full
// invoked argv to invoked_args.txt (one arg per line, via `printf '%s\n'
// "$@"`) so callers can assert the exact command shape, then writes stdout /
// stderr and exits with the given code.
func stubForgeCLI(t *testing.T, binName string, exitCode int, stdout, stderrMsg string) string {
	t.Helper()
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "invoked_args.txt")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$@" > %[1]q
cat <<'DEVSTRAP_STDOUT_EOF'
%[2]s
DEVSTRAP_STDOUT_EOF
cat <<'DEVSTRAP_STDERR_EOF' >&2
%[3]s
DEVSTRAP_STDERR_EOF
exit %[4]d
`, argsPath, stdout, stderrMsg, exitCode)
	if err := os.WriteFile(filepath.Join(dir, binName), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

// stubbedInvokedArgs reads back the argv recorded by stubForgeCLI.
func stubbedInvokedArgs(t *testing.T, dir string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "invoked_args.txt"))
	if err != nil {
		t.Fatal(err)
	}
	trimmed := strings.TrimRight(string(raw), "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

func TestCreateForgePRGitHub(t *testing.T) {
	dir := stubForgeCLI(t, "gh", 0, "https://github.com/acme/api/pull/42", "")

	out, err := createForgePR(context.Background(), t.TempDir(), "https://github.com/acme/api.git",
		"main", "agent/route-tests", "Add route tests", "Body text", "", "", nil)
	if err != nil {
		t.Fatalf("createForgePR err = %v", err)
	}
	if out != "https://github.com/acme/api/pull/42" {
		t.Fatalf("createForgePR output = %q, want the stub's stdout", out)
	}

	want := []string{"pr", "create", "--base", "main", "--head", "agent/route-tests", "--title", "Add route tests", "--body", "Body text"}
	got := stubbedInvokedArgs(t, dir)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("invoked gh argv = %v, want %v", got, want)
	}
}

func TestCreateForgePRGitLab(t *testing.T) {
	dir := stubForgeCLI(t, "glab", 0, "https://gitlab.com/acme/api/-/merge_requests/7", "")

	out, err := createForgePR(context.Background(), t.TempDir(), "https://gitlab.com/acme/api.git",
		"main", "agent/route-tests", "Add route tests", "Body text", "", "", nil)
	if err != nil {
		t.Fatalf("createForgePR err = %v", err)
	}
	if out != "https://gitlab.com/acme/api/-/merge_requests/7" {
		t.Fatalf("createForgePR output = %q, want the stub's stdout", out)
	}

	want := []string{"mr", "create", "--target-branch", "main", "--source-branch", "agent/route-tests", "--title", "Add route tests", "--description", "Body text"}
	got := stubbedInvokedArgs(t, dir)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("invoked glab argv = %v, want %v", got, want)
	}
}

// TestCreateForgePRGitea exercises the self-hosted-override path (GIT-05):
// git.example.com does not match any DetectForge heuristic (it contains
// neither "gitea." nor "codeberg.org" nor "forgejo"), so absent an override
// ResolveForge would return ForgeUnknown. Passing forgeOverride="gitea" wins
// at the highest precedence tier (flag > project column > host map >
// detection) and routes to the `tea` CLI.
func TestCreateForgePRGitea(t *testing.T) {
	if got := DetectForge(context.Background(), "https://git.example.com/acme/api.git"); got != ForgeUnknown {
		t.Fatalf("precondition: DetectForge(git.example.com) = %q, want unknown (self-hosted, undetectable)", got)
	}

	dir := stubForgeCLI(t, "tea", 0, "https://git.example.com/acme/api/pulls/3", "")

	out, err := createForgePR(context.Background(), t.TempDir(), "https://git.example.com/acme/api.git",
		"main", "agent/route-tests", "Add route tests", "Body text", "gitea", "", nil)
	if err != nil {
		t.Fatalf("createForgePR err = %v", err)
	}
	if out != "https://git.example.com/acme/api/pulls/3" {
		t.Fatalf("createForgePR output = %q, want the stub's stdout", out)
	}

	want := []string{"pr", "create", "--base", "main", "--head", "agent/route-tests", "--title", "Add route tests", "--description", "Body text"}
	got := stubbedInvokedArgs(t, dir)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("invoked tea argv = %v, want %v", got, want)
	}
}

// TestCreateForgePRCLINotFound (FORGE-01 graceful degradation): when the
// resolved forge's CLI binary is not on PATH, createForgePR must not error —
// the branch is already pushed — it should return a message naming the
// missing CLI plus a manual compare URL.
func TestCreateForgePRCLINotFound(t *testing.T) {
	// Replace PATH entirely (not prepend) so a real gh/glab/tea that happens
	// to be installed on the host cannot be found.
	t.Setenv("PATH", t.TempDir())

	remoteURL := "https://github.com/acme/api.git"
	out, err := createForgePR(context.Background(), t.TempDir(), remoteURL,
		"main", "agent/route-tests", "Add route tests", "Body text", "", "", nil)
	if err != nil {
		t.Fatalf("createForgePR err = %v, want nil (graceful degradation)", err)
	}
	if !strings.Contains(out, "agent/route-tests") || !strings.Contains(out, "pushed") {
		t.Fatalf("createForgePR output = %q, want it to mention the pushed branch", out)
	}
	if !strings.Contains(out, "gh") || !strings.Contains(out, "not found") {
		t.Fatalf("createForgePR output = %q, want it to mention gh CLI not found", out)
	}
	wantCompareURL := forgeCompareURL(context.Background(), remoteURL, "main", "agent/route-tests")
	if wantCompareURL == "" {
		t.Fatal("precondition: forgeCompareURL returned empty for a well-formed GitHub remote")
	}
	if !strings.Contains(out, wantCompareURL) {
		t.Fatalf("createForgePR output = %q, want it to include compare URL %q", out, wantCompareURL)
	}
}

// TestCreateForgePRCommandFails asserts the forge CLI's stderr is surfaced
// through a typed appError (exitGit) when the PR-create subprocess exits
// nonzero.
func TestCreateForgePRCommandFails(t *testing.T) {
	stubForgeCLI(t, "gh", 1, "", "authentication required, run `gh auth login`")

	out, err := createForgePR(context.Background(), t.TempDir(), "https://github.com/acme/api.git",
		"main", "agent/route-tests", "Add route tests", "Body text", "", "", nil)
	if err == nil {
		t.Fatalf("createForgePR err = nil, want an error; out = %q", out)
	}
	if out != "" {
		t.Errorf("createForgePR output = %q, want empty on error", out)
	}
	var ae appError
	if !errors.As(err, &ae) {
		t.Fatalf("createForgePR err = %v (%T), want an appError", err, err)
	}
	if ae.code != exitGit {
		t.Errorf("appError code = %d, want exitGit (%d)", ae.code, exitGit)
	}
	if !strings.Contains(err.Error(), "gh pr create failed") || !strings.Contains(err.Error(), "authentication required") {
		t.Errorf("createForgePR err = %q, want it to wrap the gh stderr", err.Error())
	}
}

// forgeCLIDoctorFixture provisions a workspace/device/store and a single
// adopted git_repo project pointing at remoteURL, returning the opened store
// and options ready for checkForgeCLIs.
func forgeCLIDoctorFixture(t *testing.T, remoteURL string) (context.Context, *options, *state.Store) {
	t.Helper()
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init", "--workspace-name", "personal"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}
	ctx := context.Background()
	store, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertProject(ctx, state.UpsertProjectParams{
		Path: "work/acme/api", Type: "git_repo", RemoteKey: "github.com/acme/api", RemoteURL: remoteURL,
	}); err != nil {
		t.Fatal(err)
	}
	opts := &options{v: viper.New()}
	opts.v.Set("home", home)
	opts.v.Set("root", root)
	return ctx, opts, store
}

func TestCheckForgeCLIsPresent(t *testing.T) {
	// stubForgeCLI prepends onto PATH, so a second call composes rather than
	// clobbering the first stub.
	stubForgeCLI(t, "gh", 0, "", "")
	stubForgeCLI(t, "glab", 0, "", "")

	ctx, opts, store := forgeCLIDoctorFixture(t, "https://github.com/acme/api")
	defer closeStore(store)
	if _, err := store.UpsertProject(ctx, state.UpsertProjectParams{
		Path: "work/acme/gl", Type: "git_repo", RemoteKey: "gitlab.com/acme/gl", RemoteURL: "https://gitlab.com/acme/gl",
	}); err != nil {
		t.Fatal(err)
	}

	results := checkForgeCLIs(ctx, opts, store)
	if len(results) != 0 {
		t.Fatalf("checkForgeCLIs results = %+v, want none when both gh and glab are present", results)
	}
}

func TestCheckForgeCLIsAbsent(t *testing.T) {
	// Replace PATH entirely so real gh/glab installed on the host cannot be
	// found by exec.LookPath.
	t.Setenv("PATH", t.TempDir())

	ctx, opts, store := forgeCLIDoctorFixture(t, "https://github.com/acme/api")
	defer closeStore(store)
	if _, err := store.UpsertProject(ctx, state.UpsertProjectParams{
		Path: "work/acme/gl", Type: "git_repo", RemoteKey: "gitlab.com/acme/gl", RemoteURL: "https://gitlab.com/acme/gl",
	}); err != nil {
		t.Fatal(err)
	}

	results := checkForgeCLIs(ctx, opts, store)
	if len(results) != 2 {
		t.Fatalf("checkForgeCLIs results = %+v, want 2 warnings (missing gh and glab)", results)
	}
	var sawGH, sawGlab bool
	for _, r := range results {
		if r.Status != checkWarn {
			t.Errorf("result %+v, want checkWarn status", r)
		}
		switch r.Name {
		case "forge cli gh":
			sawGH = true
			if !strings.Contains(r.Detail, "missing") || !strings.Contains(r.Detail, "github.com") {
				t.Errorf("gh result detail = %q, want it to mention missing + github.com", r.Detail)
			}
		case "forge cli glab":
			sawGlab = true
			if !strings.Contains(r.Detail, "missing") || !strings.Contains(r.Detail, "gitlab.com") {
				t.Errorf("glab result detail = %q, want it to mention missing + gitlab.com", r.Detail)
			}
		}
	}
	if !sawGH || !sawGlab {
		t.Fatalf("checkForgeCLIs results = %+v, want rows for both forge cli gh and forge cli glab", results)
	}
}
