package platform

import (
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

func TestBwrapArgsConfinesWritesAndMasksSensitivePaths(t *testing.T) {
	spec := SandboxSpec{
		WorktreeDir:        "/home/dev/agent worktree",
		TmpDir:             "/tmp/devstrap-run",
		LogDir:             "/home/dev/.devstrap/logs/agent-runs",
		UserHome:           "/home/dev",
		DevstrapHome:       "/home/dev/.devstrap",
		DenyNetwork:        true,
		DenySensitiveReads: true,
	}
	maskDirs, maskFiles := bwrapSensitivePaths(spec)
	args := bwrapArgs(spec, maskDirs, maskFiles, bwrapOptions{DisableUserns: true})

	if !reflect.DeepEqual(args[:6], []string{"--ro-bind", "/", "/", "--proc", "/proc", "--dev"}) || args[6] != "/dev" {
		t.Fatalf("first mounts = %v", args[:7])
	}
	worktreeBind := indexSequence(args, "--bind", spec.WorktreeDir, spec.WorktreeDir)
	tmpBind := indexSequence(args, "--bind", spec.TmpDir, spec.TmpDir)
	if worktreeBind == -1 || tmpBind == -1 {
		t.Fatalf("missing bind pairs in %v", args)
	}
	firstMask := len(args)
	for _, dir := range maskDirs {
		idx := indexSequence(args, "--tmpfs", dir)
		if idx == -1 {
			t.Fatalf("missing tmpfs mask for %q in %v", dir, args)
		}
		firstMask = min(firstMask, idx)
	}
	for _, file := range maskFiles {
		idx := indexSequence(args, "--ro-bind", "/dev/null", file)
		if idx == -1 {
			t.Fatalf("missing file mask for %q in %v", file, args)
		}
		firstMask = min(firstMask, idx)
	}
	if firstMask <= worktreeBind || firstMask <= tmpBind {
		t.Fatalf("masks must follow rw binds: args=%v", args)
	}
	if idx := indexSequence(args, "--unshare-user", "--disable-userns"); idx == -1 {
		t.Fatalf("--disable-userns must immediately follow --unshare-user: %v", args)
	}
	for _, want := range []string{"--unshare-pid", "--unshare-net", "--die-with-parent", "--new-session"} {
		if !slices.Contains(args, want) {
			t.Fatalf("missing %q in %v", want, args)
		}
	}
	if indexSequence(args, "--chdir", spec.WorktreeDir) == -1 {
		t.Fatalf("missing chdir worktree in %v", args)
	}
	if slices.Contains(args, spec.LogDir) {
		t.Fatalf("LogDir leaked into bwrap args: %v", args)
	}
	if !slices.Contains(args, spec.WorktreeDir) {
		t.Fatalf("space-containing worktree path missing as argv element: %v", args)
	}
}

func TestBwrapArgsOmitsOptionalPieces(t *testing.T) {
	args := bwrapArgs(SandboxSpec{
		WorktreeDir: "/wt",
		TmpDir:      "/tmp",
	}, nil, nil, bwrapOptions{})
	for _, forbidden := range []string{"--unshare-net", "--disable-userns", "--tmpfs"} {
		if slices.Contains(args, forbidden) {
			t.Fatalf("%q leaked into optional-minimal args: %v", forbidden, args)
		}
	}
	if indexSequence(args, "--ro-bind", "/dev/null") != -1 {
		t.Fatalf("/dev/null file mask leaked into optional-minimal args: %v", args)
	}
}

func TestBwrapSensitivePathsMirrorsSeatbeltDenyList(t *testing.T) {
	dirs, files := bwrapSensitivePaths(SandboxSpec{
		UserHome:           "/home/dev",
		DevstrapHome:       "/home/dev/.devstrap",
		DenySensitiveReads: true,
	})
	var wantDirs []string
	for _, rel := range sensitiveHomeDirs {
		wantDirs = append(wantDirs, filepath.Join("/home/dev", rel))
	}
	wantDirs = append(wantDirs, "/home/dev/.devstrap/keys")
	var wantFiles []string
	for _, rel := range sensitiveHomeFiles {
		wantFiles = append(wantFiles, filepath.Join("/home/dev", rel))
	}
	if !reflect.DeepEqual(dirs, wantDirs) {
		t.Fatalf("dirs = %v, want %v", dirs, wantDirs)
	}
	if !reflect.DeepEqual(files, wantFiles) {
		t.Fatalf("files = %v, want %v", files, wantFiles)
	}

	dirs, files = bwrapSensitivePaths(SandboxSpec{DenySensitiveReads: false})
	if dirs != nil || files != nil {
		t.Fatalf("bwrapSensitivePaths without DenySensitiveReads = %v, %v; want nil, nil", dirs, files)
	}
	dirs, files = bwrapSensitivePaths(SandboxSpec{
		DevstrapHome:       "/devstrap",
		DenySensitiveReads: true,
	})
	if !reflect.DeepEqual(dirs, []string{"/devstrap/keys"}) || files != nil {
		t.Fatalf("empty UserHome paths = %v, %v; want devstrap keys only", dirs, files)
	}
}

func indexSequence(args []string, seq ...string) int {
	for i := 0; i+len(seq) <= len(args); i++ {
		if slices.Equal(args[i:i+len(seq)], seq) {
			return i
		}
	}
	return -1
}
