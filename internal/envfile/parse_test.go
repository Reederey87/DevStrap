package envfile

import "testing"

// FuzzParseBytes (QUAL-01): the .env parser sits on the untrusted boundary.
// It must never panic on arbitrary input; rejecting malformed input with an
// error is fine. Seeds are drawn from the existing table-test corpus.
func FuzzParseBytes(f *testing.F) {
	f.Add([]byte("FOO=bar\nBAZ=\"qux\"\n# comment\n"))
	f.Add([]byte("EMPTY=\n"))
	f.Add([]byte("MULTI='a\\nb'\n"))
	f.Add([]byte("=noval\n"))
	f.Add([]byte("BAD-NAME=value\n"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		// Bound the input so the fuzzer does not spend budget on pathologically
		// large inputs (the parser already caps at MaxBytes).
		if len(raw) > 64*1024 {
			t.Skip()
		}
		_, err := ParseBytes(raw, Options{})
		_ = err // rejecting bad input is acceptable; panicking is not
	})
}
