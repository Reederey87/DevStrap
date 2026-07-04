package ignore

import "testing"

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
