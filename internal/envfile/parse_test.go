package envfile

import (
	"strings"
	"testing"
)

func TestParseBytesSupportsDocumentedDotenvGrammar(t *testing.T) {
	raw := []byte(strings.Join([]string{
		"# comment",
		"export API_KEY=abc123",
		"EMPTY=",
		"URL=https://example.test/a=b",
		"PASSWORD=p#ss # kept until whitespace hash",
		"COMMENTED=value # comment",
		"SINGLE='literal \\n # value'",
		`DOUBLE="line\nquoted \"value\" # kept"`,
		"",
	}, "\r\n"))

	bindings, err := ParseBytes(raw, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := ToSecretMap(bindings)
	want := map[string]string{
		"API_KEY":   "abc123",
		"EMPTY":     "",
		"URL":       "https://example.test/a=b",
		"PASSWORD":  "p#ss",
		"COMMENTED": "value",
		"SINGLE":    `literal \n # value`,
		"DOUBLE":    "line\nquoted \"value\" # kept",
	}
	for name, value := range want {
		if got[name] != value {
			t.Fatalf("%s = %q, want %q; all=%#v", name, got[name], value, got)
		}
	}
}

func TestParseBytesRejectsInterpolationUnlessLiteral(t *testing.T) {
	if _, err := ParseBytes([]byte("TOKEN=${OTHER}\n"), Options{}); err == nil {
		t.Fatal("ParseBytes accepted interpolation without literal mode")
	}
	bindings, err := ParseBytes([]byte("TOKEN=${OTHER}\n"), Options{Literal: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := ToSecretMap(bindings)["TOKEN"]; got != "${OTHER}" {
		t.Fatalf("TOKEN = %q, want literal interpolation text", got)
	}
}

func TestParseBytesRejectsDangerousNamesAndDuplicates(t *testing.T) {
	if _, err := ParseBytes([]byte("NODE_OPTIONS=--require ./hook.js\n"), Options{}); err == nil {
		t.Fatal("ParseBytes accepted dangerous name")
	}
	if _, err := ParseBytes([]byte("TOKEN=one\nTOKEN=two\n"), Options{}); err == nil {
		t.Fatal("ParseBytes accepted duplicate name")
	}
}

func TestParseBytesRejectsMalformedLines(t *testing.T) {
	for _, raw := range []string{
		"NO_EQUALS\n",
		"1BAD=value\n",
		"SINGLE='unterminated\n",
		`DOUBLE="bad\q"` + "\n",
		`DOUBLE="ok" trailing` + "\n",
	} {
		if _, err := ParseBytes([]byte(raw), Options{}); err == nil {
			t.Fatalf("ParseBytes(%q) succeeded, want error", raw)
		}
	}
}

func TestParseBytesRejectsOversizedInput(t *testing.T) {
	if _, err := ParseBytes(make([]byte, MaxBytes+1), Options{}); err == nil {
		t.Fatal("ParseBytes accepted oversized input")
	}
}
