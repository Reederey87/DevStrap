package pathkey

import (
	"strings"
	"testing"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// FuzzClean pins the pathkey normalizer properties that keep one project from
// splitting into two path_keys across devices: idempotence, NFC Display, no
// escape/empty/absolute segments, and a case-folded Key that is stable under
// ASCII case variation of the input.
func FuzzClean(f *testing.F) {
	for _, seed := range []string{
		"",
		"a",
		"a/b",
		"A/B",
		"a//b",
		"./a",
		"../a",
		"/a",
		"a/../b",
		"a/.",
		"café",       // NFC
		"cafe\u0301", // NFD of the same string
		"a\\b",
		"a\x00b",
		" a ",
		"a/b/",
		strings.Repeat("a/", 100),
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, in string) {
		got, err := Clean(in)
		if err != nil {
			return // a rejection is a valid outcome; only accepted output is constrained
		}

		// (a) idempotence — a non-idempotent normalizer yields two path_keys
		// for one path, which splits one project into two across devices.
		again, err2 := Clean(got.Display)
		if err2 != nil {
			t.Fatalf("Clean rejected its own Display %q: %v", got.Display, err2)
		}
		if again != got {
			t.Fatalf("not idempotent: Clean(%q)=%+v, Clean(%q)=%+v", in, got, got.Display, again)
		}

		// (b) Display is NFC. Do not assert Key is NFC: Key is ToLower'd after
		// normalization, and ToLower can emit a decomposed sequence (İ → i +
		// combining dot).
		if !norm.NFC.IsNormalString(got.Display) {
			t.Fatalf("Display is not NFC: %q (Key=%q)", got.Display, got.Key)
		}

		// (c) Display has no ".." segment, no empty segment, and is not absolute.
		if strings.HasPrefix(got.Display, "/") {
			t.Fatalf("Display is absolute: %q", got.Display)
		}
		for _, part := range strings.Split(got.Display, "/") {
			if part == "" {
				t.Fatalf("Display has empty segment: %q", got.Display)
			}
			if part == ".." {
				t.Fatalf("Display has .. segment: %q", got.Display)
			}
		}

		// (d) Key is the case-folded Display, re-normalized. Asserting the bare
		// `Key == strings.ToLower(Display)` would merely restate the
		// implementation — and that spelling is what the pre-fix code did, which
		// is the bug this target found. The property worth pinning is that Key
		// is itself NFC, since a non-NFC key is one that a canonically
		// equivalent path will not match.
		if !norm.NFC.IsNormalString(got.Key) {
			t.Fatalf("Key is not NFC: %q (Display=%q)", got.Key, got.Display)
		}
		if got.Key != norm.NFC.String(cases.Fold().String(got.Display)) {
			t.Fatalf("Key is not the re-normalized case-fold of Display: %+v", got)
		}
		// ASCII case variation of the input must produce the same Key —
		// path_key is the identity two devices compare.
		flipped := asciiFlipCase(in)
		gotFlip, errFlip := Clean(flipped)
		if errFlip != nil {
			t.Fatalf("ASCII case-variant rejected: in=%q flipped=%q err=%v (accepted=%+v)",
				in, flipped, errFlip, got)
		}
		if gotFlip.Key != got.Key {
			t.Fatalf("ASCII case-variant Key mismatch: in=%q Key=%q; flipped=%q Key=%q",
				in, got.Key, flipped, gotFlip.Key)
		}
	})
}

// asciiFlipCase flips only ASCII a-z/A-Z bytes. strings.ToUpper is not used:
// for non-ASCII it can change rune counts and normalization, which would make
// the test fail for reasons that are not the property under test.
func asciiFlipCase(s string) string {
	b := []byte(s)
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r < utf8.RuneSelf {
			c := b[i]
			switch {
			case c >= 'a' && c <= 'z':
				b[i] = c - 'a' + 'A'
			case c >= 'A' && c <= 'Z':
				b[i] = c - 'A' + 'a'
			}
			i++
			continue
		}
		i += size
	}
	return string(b)
}
