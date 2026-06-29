package ignore

import "testing"

// FuzzCompile (QUAL-01): the .devstrapignore compiler turns user input into
// regexps. It must never panic and the resulting matcher must always terminate
// on Match/ShouldPruneDir. Seeds are drawn from the existing test corpus.
func FuzzCompile(f *testing.F) {
	f.Add("node_modules/\n!keep/**\n/a/b?c\n")
	f.Add("**/.env\n")
	f.Add("[abc]/x\n")
	f.Add("*.log\n")
	f.Add("")
	f.Fuzz(func(t *testing.T, src string) {
		m, err := Compile(src, true)
		if err != nil {
			return // rejecting a bad pattern is fine; panicking is not
		}
		_ = m.Match("a/b/c", false)
		_ = m.Match("a/b/c", true)
		_ = m.ShouldPruneDir("x", "a/x")
	})
}
