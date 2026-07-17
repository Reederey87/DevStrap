package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParsePorcelainV2(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   Gitstate
	}{
		{
			name:   "clean no upstream",
			output: "# branch.oid abc123\n# branch.head main\n",
			want:   Gitstate{Branch: "main", HeadSHA: "abc123"},
		},
		{
			name:   "clean with upstream",
			output: "# branch.oid abc123\n# branch.head main\n# branch.upstream origin/main\n# branch.ab +0 -0\n",
			want:   Gitstate{Branch: "main", HeadSHA: "abc123", UpstreamBranch: "origin/main"},
		},
		{
			name: "dirty with untracked",
			output: "# branch.oid abc123\n# branch.head main\n# branch.upstream origin/main\n# branch.ab +0 -0\n" +
				"1 .M N... 100644 100644 100644 aaa bbb file.go\n? new.go\n",
			want: Gitstate{Branch: "main", HeadSHA: "abc123", UpstreamBranch: "origin/main", DirtyCount: 1, UntrackedCount: 1},
		},
		{
			name:   "renamed counts as dirty",
			output: "# branch.oid abc123\n# branch.head main\n2 R. N... 100644 100644 100644 aaa bbb R100 new.go\told.go\n",
			want:   Gitstate{Branch: "main", HeadSHA: "abc123", DirtyCount: 1},
		},
		{
			name:   "unmerged conflict",
			output: "# branch.oid abc123\n# branch.head main\nu UU N... 100644 100644 100644 100644 aaa bbb ccc file.go\n",
			want:   Gitstate{Branch: "main", HeadSHA: "abc123", UnmergedCount: 1},
		},
		{
			name:   "ahead",
			output: "# branch.oid abc123\n# branch.head main\n# branch.upstream origin/main\n# branch.ab +2 -0\n",
			want:   Gitstate{Branch: "main", HeadSHA: "abc123", UpstreamBranch: "origin/main", AheadCount: 2},
		},
		{
			name:   "behind",
			output: "# branch.oid abc123\n# branch.head main\n# branch.upstream origin/main\n# branch.ab +0 -3\n",
			want:   Gitstate{Branch: "main", HeadSHA: "abc123", UpstreamBranch: "origin/main", BehindCount: 3},
		},
		{
			name:   "diverged",
			output: "# branch.oid abc123\n# branch.head main\n# branch.upstream origin/main\n# branch.ab +2 -3\n",
			want:   Gitstate{Branch: "main", HeadSHA: "abc123", UpstreamBranch: "origin/main", AheadCount: 2, BehindCount: 3},
		},
		{
			name:   "detached head",
			output: "# branch.oid abc123\n# branch.head (detached)\n",
			want:   Gitstate{HeadSHA: "abc123"},
		},
		{
			name:   "initial unborn branch",
			output: "# branch.oid (initial)\n# branch.head main\n",
			want:   Gitstate{Branch: "main"},
		},
		{
			name:   "unrecognized line ignored",
			output: "# branch.oid abc123\n# branch.head main\n! ignored.txt\n",
			want:   Gitstate{Branch: "main", HeadSHA: "abc123"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parsePorcelainV2(tc.output)
			if got != tc.want {
				t.Fatalf("parsePorcelainV2() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// writeFakeGitDispatch builds a fake git executable that branches on the first
// non-option argument (skipping secureArgs' "-c k v" pairs and
// --no-optional-locks), mirroring the dispatch pattern used by the
// ResolveDefaultBranch fakes elsewhere in this package.
func writeFakeGitDispatch(t *testing.T, cases string) string {
	t.Helper()
	return writeFakeGit(t, fmt.Sprintf(`#!/bin/sh
sub=""
while [ $# -gt 0 ]; do
  case "$1" in
    -c) shift 2;;
    --no-optional-locks) shift;;
    *) sub="$1"; shift; break;;
  esac
done
case "$sub" in
%s
esac
`, cases))
}

func TestCaptureGitstateResolvesUpstreamSHAAndStashCount(t *testing.T) {
	script := writeFakeGitDispatch(t, `
  status) cat <<'EOF'
# branch.oid abc123
# branch.head main
# branch.upstream origin/main
# branch.ab +1 -0
1 .M N... 100644 100644 100644 aaa bbb file.go
EOF
  ;;
  rev-parse) echo "def456"; exit 0;;
  stash) printf 'stash@{0}: WIP on main: abc123 msg\nstash@{1}: WIP on main: def456 msg\n'; exit 0;;
  *) exit 0;;
`)
	r := Runner{Bin: script, Timeout: 5 * time.Second}
	got, err := r.CaptureGitstate(context.Background(), "")
	if err != nil {
		t.Fatalf("CaptureGitstate err = %v", err)
	}
	want := Gitstate{
		Branch: "main", HeadSHA: "abc123", UpstreamBranch: "origin/main", UpstreamSHA: "def456",
		DirtyCount: 1, AheadCount: 1, StashCount: 2,
	}
	if got != want {
		t.Fatalf("CaptureGitstate() = %+v, want %+v", got, want)
	}
}

func TestCaptureGitstateNoUpstreamSkipsRevParseAndLeavesUpstreamSHAEmpty(t *testing.T) {
	script := writeFakeGitDispatch(t, `
  status) cat <<'EOF'
# branch.oid abc123
# branch.head main
EOF
  ;;
  rev-parse) echo "FAIL: should not be called without an upstream" >&2; exit 1;;
  stash) exit 0;;
  *) exit 0;;
`)
	r := Runner{Bin: script, Timeout: 5 * time.Second}
	got, err := r.CaptureGitstate(context.Background(), "")
	if err != nil {
		t.Fatalf("CaptureGitstate err = %v", err)
	}
	if got.UpstreamBranch != "" || got.UpstreamSHA != "" {
		t.Fatalf("CaptureGitstate() = %+v, want no upstream", got)
	}
	if got.StashCount != 0 {
		t.Fatalf("StashCount = %d, want 0", got.StashCount)
	}
}

func TestCaptureGitstateStaleUpstreamRefLeavesUpstreamSHAEmpty(t *testing.T) {
	script := writeFakeGitDispatch(t, `
  status) cat <<'EOF'
# branch.oid abc123
# branch.head main
# branch.upstream origin/gone
# branch.ab +0 -0
EOF
  ;;
  rev-parse) echo "fatal: ambiguous argument 'origin/gone'" >&2; exit 128;;
  stash) exit 0;;
  *) exit 0;;
`)
	r := Runner{Bin: script, Timeout: 5 * time.Second}
	got, err := r.CaptureGitstate(context.Background(), "")
	if err != nil {
		t.Fatalf("CaptureGitstate err = %v", err)
	}
	if got.UpstreamBranch != "origin/gone" || got.UpstreamSHA != "" {
		t.Fatalf("CaptureGitstate() = %+v, want upstream branch recorded but sha empty", got)
	}
}

func TestCaptureGitstateOnRealRepo(t *testing.T) {
	dir := t.TempDir()
	r := NewRunner()
	r.Timeout = 10 * time.Second
	ctx := context.Background()
	run := func(args ...string) {
		t.Helper()
		if _, err := r.Run(ctx, dir, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "file.txt")
	run("commit", "-m", "initial")

	gs, err := r.CaptureGitstate(ctx, dir)
	if err != nil {
		t.Fatalf("CaptureGitstate err = %v", err)
	}
	if gs.Branch != "main" || gs.HeadSHA == "" {
		t.Fatalf("CaptureGitstate() = %+v, want branch main with a head sha", gs)
	}
	if gs.DirtyCount != 0 || gs.UntrackedCount != 0 || gs.StashCount != 0 {
		t.Fatalf("CaptureGitstate() = %+v, want clean working tree", gs)
	}

	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gs, err = r.CaptureGitstate(ctx, dir)
	if err != nil {
		t.Fatalf("CaptureGitstate err = %v", err)
	}
	if gs.DirtyCount != 1 || gs.UntrackedCount != 1 {
		t.Fatalf("CaptureGitstate() = %+v, want one dirty and one untracked file", gs)
	}
}
