package ignore

import (
	"strings"
	"testing"
)

// Explicit NFC/NFD forms of "café" so tests never depend on source-file normalization.
const (
	cafeNFC = "caf\u00e9"  // U+00E9 LATIN SMALL LETTER E WITH ACUTE
	cafeNFD = "cafe\u0301" // e + U+0301 COMBINING ACUTE ACCENT
)

func TestMatchDefaults(t *testing.T) {
	m := DefaultMatcher()
	tests := []struct {
		path  string
		isDir bool
		want  bool
	}{
		{"node_modules", true, true},
		{"node_modules/pkg/index.js", false, true}, // descendant of dir-only match
		{"src/node_modules", true, true},           // unanchored
		{"dist/app.js", false, true},
		{".DS_Store", false, true},
		{"src/main.go", false, false},
		{"go.mod", false, false},
		{".git", true, true},
		{".git/config", false, true},
		{"__pycache__/foo.pyc", false, true},
		{".venv/bin/python", false, true},
		{"target/release/app", false, true},
		{"data/raw", true, true},             // default now `**/data/raw/` → pruned at root…
		{"experiments/data/raw", true, true}, // …and at any depth (prune-anywhere intent preserved)
		{"README.md", false, false},
	}
	for _, tc := range tests {
		got := m.Match(tc.path, tc.isDir)
		if got != tc.want {
			t.Errorf("Match(%q, isDir=%v) = %v, want %v", tc.path, tc.isDir, got, tc.want)
		}
	}
}

func TestCompileUserPatterns(t *testing.T) {
	src := "# comment\n\n*.log\n!important.log\n/build/\n**/tmp/\n"
	m, err := Compile(src, false)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		path  string
		isDir bool
		want  bool
	}{
		{"app.log", false, true},
		{"logs/run.log", false, true},
		{"important.log", false, false}, // negated
		{"build", true, true},           // anchored dir-only
		{"src/build", true, false},      // anchored, not at root
		{"foo/tmp", true, true},         // **/tmp/
		{"tmp", true, true},             // **/tmp/ matches at root too
		{"src/main.go", false, false},
	}
	for _, tc := range tests {
		got := m.Match(tc.path, tc.isDir)
		if got != tc.want {
			t.Errorf("Match(%q, isDir=%v) = %v, want %v", tc.path, tc.isDir, got, tc.want)
		}
	}
}

func TestCompileIncludesDefaults(t *testing.T) {
	m, err := Compile("*.log\n", true)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Match("node_modules", true) {
		t.Error("defaults not included with includeDefaults=true")
	}
	if !m.Match("app.log", false) {
		t.Error("user pattern not applied")
	}
}

func TestShouldPruneDir(t *testing.T) {
	m := DefaultMatcher()
	if !m.ShouldPruneDir("node_modules", "work/app/node_modules") {
		t.Error("ShouldPruneDir(node_modules) = false, want true")
	}
	if !m.ShouldPruneDir(".git", "work/app/.git") {
		t.Error("ShouldPruneDir(.git) = false, want true")
	}
	if m.ShouldPruneDir("src", "work/app/src") {
		t.Error("ShouldPruneDir(src) = true, want false")
	}
}

// TestShouldPruneDirAnchoredPatternDoesNotPruneNested guards against a bare-name
// fallback reintroduction: an anchored pattern like "/dist/" must only match
// the root-level "dist" directory, never a nested directory that merely
// shares its base name.
func TestShouldPruneDirAnchoredPatternDoesNotPruneNested(t *testing.T) {
	m, err := Compile("/dist/\n", false)
	if err != nil {
		t.Fatal(err)
	}
	if m.ShouldPruneDir("dist", "packages/app/dist") {
		t.Error(`ShouldPruneDir("dist", "packages/app/dist") = true, want false (anchored pattern must not match nested dir)`)
	}
}

// TestShouldPruneDirNegationReincludes guards against a bare-name fallback
// re-ignoring a path that a later negation pattern explicitly re-included.
func TestShouldPruneDirNegationReincludes(t *testing.T) {
	m, err := Compile("build/\n!keep/build/\n", false)
	if err != nil {
		t.Fatal(err)
	}
	if m.ShouldPruneDir("build", "keep/build") {
		t.Error(`ShouldPruneDir("build", "keep/build") = true, want false (negation must re-include)`)
	}
}

// TestShouldPruneDirRootLevelStillPruned confirms the fix does not regress the
// common case: an anchored pattern still prunes the matching top-level dir.
func TestShouldPruneDirRootLevelStillPruned(t *testing.T) {
	m, err := Compile("/dist/\n", false)
	if err != nil {
		t.Fatal(err)
	}
	if !m.ShouldPruneDir("dist", "dist") {
		t.Error(`ShouldPruneDir("dist", "dist") = false, want true (anchored pattern must still prune root-level match)`)
	}
}

func TestGitignoreFragmentRoundTrip(t *testing.T) {
	m := DefaultMatcher()
	frag := m.GitignoreFragment()
	if frag == "" {
		t.Fatal("empty gitignore fragment")
	}
	m2, err := Compile(frag, false)
	if err != nil {
		t.Fatalf("recompile fragment: %v", err)
	}
	if !m2.Match("node_modules", true) {
		t.Error("recompiled fragment lost node_modules")
	}
}

func TestInvalidPattern(t *testing.T) {
	if _, err := Compile("/\n", false); err == nil {
		t.Error("expected error for empty pattern after stripping")
	}
}

// TestMatchNFCPatternMatchesNFDPath: NFC pattern café/ must ignore NFD café paths
// (NFD names arrive from HFS+ legacy volumes, archives, network filesystems, and
// NFD-writing apps; .devstrapignore usually carries NFC) — P7-XP-04.
func TestMatchNFCPatternMatchesNFDPath(t *testing.T) {
	m, err := Compile(cafeNFC+"/\n", false)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Match(cafeNFD, true) {
		t.Errorf("Match(%q, isDir=true) = false, want true (NFC pattern vs NFD path)", cafeNFD)
	}
	// dirOnly descendant matching: file inside the ignored directory.
	inside := cafeNFD + "/menu.txt"
	if !m.Match(inside, false) {
		t.Errorf("Match(%q, isDir=false) = false, want true (descendant of dir-only match)", inside)
	}
}

// TestMatchNFDPatternMatchesNFCPath: reverse of the above — editor saved NFD
// into .devstrapignore, path is NFC (Linux) — P7-XP-04.
func TestMatchNFDPatternMatchesNFCPath(t *testing.T) {
	m, err := Compile(cafeNFD+"/\n", false)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Match(cafeNFC, true) {
		t.Errorf("Match(%q, isDir=true) = false, want true (NFD pattern vs NFC path)", cafeNFC)
	}
}

// TestShouldPruneDirNFDName: empty relSlash falls back to name; both forms must
// prune after NFC normalization through Match — P7-XP-04.
func TestShouldPruneDirNFDName(t *testing.T) {
	m, err := Compile(cafeNFC+"/\n", false)
	if err != nil {
		t.Fatal(err)
	}
	if !m.ShouldPruneDir(cafeNFD, cafeNFD) {
		t.Errorf(`ShouldPruneDir(%q, %q) = false, want true`, cafeNFD, cafeNFD)
	}
	// relSlash == "" fallback to name also flows through Match (and thus NFC).
	if !m.ShouldPruneDir(cafeNFD, "") {
		t.Errorf(`ShouldPruneDir(%q, "") = false, want true (name fallback)`, cafeNFD)
	}
}

// TestGitignoreFragmentEmitsNFC: NFD pattern compiles to NFC text in the
// fragment; recompile round-trips to an equal matcher — P7-XP-04.
func TestGitignoreFragmentEmitsNFC(t *testing.T) {
	m, err := Compile(cafeNFD+"/\n", false)
	if err != nil {
		t.Fatal(err)
	}
	frag := m.GitignoreFragment()
	if !strings.Contains(frag, cafeNFC+"/") {
		t.Errorf("GitignoreFragment() = %q, want to contain NFC %q", frag, cafeNFC+"/")
	}
	if strings.Contains(frag, cafeNFD) {
		t.Errorf("GitignoreFragment() still contains NFD form %q: %q", cafeNFD, frag)
	}
	m2, err := Compile(frag, false)
	if err != nil {
		t.Fatalf("recompile fragment: %v", err)
	}
	// Round-trip: same ignore decisions on NFC and NFD inputs.
	for _, path := range []string{cafeNFC, cafeNFD} {
		if m.Match(path, true) != m2.Match(path, true) {
			t.Errorf("round-trip mismatch for %q: orig=%v recompiled=%v",
				path, m.Match(path, true), m2.Match(path, true))
		}
		if !m2.Match(path, true) {
			t.Errorf("recompiled matcher Match(%q, true) = false, want true", path)
		}
	}
}

// TestNegationWinsAfterNormalization: café/ + !café/keep/ with NFD paths —
// last-match-wins negation still applies after NFC — P7-XP-04.
func TestNegationWinsAfterNormalization(t *testing.T) {
	src := cafeNFC + "/\n!" + cafeNFC + "/keep/\n"
	m, err := Compile(src, false)
	if err != nil {
		t.Fatal(err)
	}
	// NFD café dir is ignored.
	if !m.Match(cafeNFD, true) {
		t.Errorf("Match(%q, true) = false, want true", cafeNFD)
	}
	// NFD café/keep is re-included by negation.
	keep := cafeNFD + "/keep"
	if m.Match(keep, true) {
		t.Errorf("Match(%q, true) = true, want false (negation must re-include)", keep)
	}
	if m.ShouldPruneDir("keep", keep) {
		t.Errorf(`ShouldPruneDir("keep", %q) = true, want false`, keep)
	}
}
