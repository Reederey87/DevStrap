package pathkey

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCleanRejectsUnsafePaths(t *testing.T) {
	tests := []struct {
		name string
		in   string
		err  error
	}{
		{"empty", "", ErrEmpty},
		{"absolute", "/tmp/repo", ErrAbsolute},
		{"escape", "../repo", ErrEscape},
		{"empty part", "work//repo", ErrEmptyPart},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Clean(tt.in)
			if !errors.Is(err, tt.err) {
				t.Fatalf("Clean(%q) error = %v, want %v", tt.in, err, tt.err)
			}
		})
	}
}

func TestCleanReturnsDisplayAndCaseFoldedKey(t *testing.T) {
	got, err := Clean("Work/Org/Repo")
	if err != nil {
		t.Fatal(err)
	}
	if got.Display != "Work/Org/Repo" || got.Key != "work/org/repo" {
		t.Fatalf("Clean returned %+v", got)
	}
}

func TestCleanNormalizesUnicodeToNFC(t *testing.T) {
	got, err := Clean("work/cafe\u0301")
	if err != nil {
		t.Fatal(err)
	}
	if got.Display != "work/café" || got.Key != "work/café" {
		t.Fatalf("Clean returned %+v, want NFC display/key", got)
	}
	// NFC and NFD spellings of the same name must collapse to one key so
	// duplicate detection and cross-device sync match (macOS NFD vs Linux NFC).
	nfc, err := Clean("work/café") // é precomposed (NFC)
	if err != nil {
		t.Fatal(err)
	}
	nfd, err := Clean("work/café") // e + combining acute (NFD)
	if err != nil {
		t.Fatal(err)
	}
	if nfc.Key != nfd.Key {
		t.Fatalf("NFC key %q != NFD key %q", nfc.Key, nfd.Key)
	}
}

func TestDetectCaseConflicts(t *testing.T) {
	a, _ := Clean("work/API")
	b, _ := Clean("work/api")
	if err := DetectCaseConflicts([]Path{a, b}); !errors.Is(err, ErrPathConflict) {
		t.Fatalf("expected ErrPathConflict for case-only collision, got %v", err)
	}
	c, _ := Clean("work/other")
	if err := DetectCaseConflicts([]Path{a, c}); err != nil {
		t.Fatalf("distinct paths should not conflict: %v", err)
	}
}

func TestCheckSymlinkWithinRoot(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "real")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	within := filepath.Join(root, "within-link")
	if err := os.Symlink(inside, within); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := CheckSymlinkWithinRoot(root, within); err != nil {
		t.Fatalf("within-root symlink should pass: %v", err)
	}

	outside := t.TempDir()
	escape := filepath.Join(root, "escape-link")
	if err := os.Symlink(outside, escape); err != nil {
		t.Fatal(err)
	}
	if err := CheckSymlinkWithinRoot(root, escape); !errors.Is(err, ErrEscape) {
		t.Fatalf("escaping symlink should return ErrEscape, got %v", err)
	}

	dangling := filepath.Join(root, "dangling-link")
	if err := os.Symlink(filepath.Join(root, "does-not-exist"), dangling); err != nil {
		t.Fatal(err)
	}
	if err := CheckSymlinkWithinRoot(root, dangling); !errors.Is(err, ErrDangling) {
		t.Fatalf("dangling symlink should return ErrDangling, got %v", err)
	}
}

func TestVerifyWithinRoot(t *testing.T) {
	root := t.TempDir()
	// A not-yet-created target whose parent is within root is allowed.
	target := filepath.Join(root, "work", "repo")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := VerifyWithinRoot(root, target); err != nil {
		t.Fatalf("in-root target should pass: %v", err)
	}
	// A nested target whose intermediate dirs do not exist yet (peer device
	// before skeleton reconciliation) must still pass — only the existing
	// portion of the path is checked.
	nested := filepath.Join(root, "work", "org", "deep", "repo")
	if err := VerifyWithinRoot(root, nested); err != nil {
		t.Fatalf("nested not-yet-created target should pass: %v", err)
	}
	// A target reached via a symlink repointed outside the root is rejected.
	outside := t.TempDir()
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	escaped := filepath.Join(link, "repo")
	if err := VerifyWithinRoot(root, escaped); !errors.Is(err, ErrEscape) {
		t.Fatalf("target via escaping symlink should return ErrEscape, got %v", err)
	}
}
