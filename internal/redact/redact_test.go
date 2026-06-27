package redact

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// secretLike builds a secret-shaped value at runtime from a prefix and body so
// that no contiguous secret literal is ever committed to source (GitHub push
// protection scans source text). The scrubber regexes still see the full value
// at runtime.
func secretLike(prefix, body string) string { return prefix + body }

func TestSecretNeverRendersPlaintext(t *testing.T) {
	plaintext := secretLike("ghp_", "supersecretvalue1234567890ABCD")
	s := New(plaintext)

	rendered := []string{
		s.String(),
		s.GoString(),
		fmt.Sprintf("%v", s),
		fmt.Sprintf("token=%s", s),
		fmt.Sprintf("%#v", s),
	}
	if b, err := s.MarshalText(); err != nil {
		t.Fatalf("MarshalText: %v", err)
	} else {
		rendered = append(rendered, string(b))
	}
	if b, err := json.Marshal(s); err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	} else {
		rendered = append(rendered, string(b))
	}
	if b, err := json.Marshal(struct {
		Token Secret `json:"token"`
	}{Token: s}); err != nil {
		t.Fatalf("nested MarshalJSON: %v", err)
	} else {
		rendered = append(rendered, string(b))
	}
	rendered = append(rendered, s.LogValue().String())

	for _, out := range rendered {
		if strings.Contains(out, plaintext) {
			t.Fatalf("plaintext leaked in %q", out)
		}
		if !strings.Contains(out, Placeholder) {
			t.Fatalf("expected placeholder in %q", out)
		}
	}

	if s.Reveal() != plaintext {
		t.Fatalf("Reveal() = %q, want plaintext", s.Reveal())
	}
}

func TestSecretLogValuerRedactsUnderBenignKey(t *testing.T) {
	ageKey := secretLike("AGE-SECRET-KEY-1", strings.Repeat("Q", 50)+"Z")
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	logger.Info("event", "value", New(ageKey))
	out := buf.String()
	if strings.Contains(out, "AGE-SECRET-KEY-1") {
		t.Fatalf("age secret key leaked in log: %q", out)
	}
	if !strings.Contains(out, Placeholder) {
		t.Fatalf("expected placeholder in log: %q", out)
	}
}

func TestURLStripsCredentials(t *testing.T) {
	token := secretLike("ghp_", "token1234567890abcdef")
	cases := []struct {
		in   string
		bad  string
		want string
	}{
		{"https://user:" + token + "@github.com/org/repo.git", token, "github.com/org/repo.git"},
		{"https://x-access-token:secretsecret@github.com/o/r.git", "secretsecret", "github.com"},
		{"ssh://git@example.com/repo.git", "", "example.com"},
	}
	for _, c := range cases {
		got := URL(c.in)
		if c.bad != "" && strings.Contains(got, c.bad) {
			t.Fatalf("URL(%q) leaked credential: %q", c.in, got)
		}
		if !strings.Contains(got, c.want) {
			t.Fatalf("URL(%q) = %q, want it to contain %q", c.in, got, c.want)
		}
	}
}

func TestScrubTokenShapes(t *testing.T) {
	jwt := secretLike("eyJ", "hbGciOiJIUzI1NiJ9") + "." + secretLike("eyJ", "zdWIiOiIxMjM0In0") + "." + "dummysignature123"
	secrets := []string{
		secretLike("ghp_", "abcdefghijklmnopqrstuvwxyz0123456789"),
		secretLike("github_pat_", "11ABCDEFG0abcdefghijklmnop"),
		secretLike("xoxb-", "123456789012-1234567890123-abcdefghijkl"),
		secretLike("AKIA", strings.Repeat("Q", 16)),
		secretLike("sk-", "abcdefghijklmnopqrstuvwxyz0123456789"),
		jwt,
	}
	for _, secret := range secrets {
		text := "context before " + secret + " context after"
		got := Scrub(text)
		if strings.Contains(got, secret) {
			t.Fatalf("Scrub failed to redact %q: %q", secret, got)
		}
		if !strings.Contains(got, "context before") || !strings.Contains(got, "context after") {
			t.Fatalf("Scrub mangled surrounding text: %q", got)
		}
	}
}

func TestStripURLUserinfo(t *testing.T) {
	httpsToken := secretLike("ghp_", "tokenabcdef1234567890")
	cases := []struct {
		name    string
		in      string
		leak    string
		wantHas string
	}{
		// https: drop the whole userinfo (it can only be a credential).
		{"https creds", "https://x-access-token:" + httpsToken + "@github.com/o/r.git", httpsToken, "https://github.com/o/r.git"},
		// ssh: keep the login name (git@), it is not a secret.
		{"ssh login", "ssh://git@github.com/o/r.git", "", "ssh://git@github.com/o/r.git"},
		{"ssh login port", "ssh://git@gitlab.com:2222/o/r.git", "", "ssh://git@gitlab.com:2222/o/r.git"},
		// ssh with a password: drop only the password, keep the login.
		{"ssh password", "ssh://git:" + httpsToken + "@host/r.git", httpsToken, "ssh://git@host/r.git"},
		// no userinfo: unchanged.
		{"no creds", "https://github.com/o/r.git", "", "https://github.com/o/r.git"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := StripURLUserinfo(c.in)
			if c.leak != "" && strings.Contains(got, c.leak) {
				t.Fatalf("StripURLUserinfo(%q) leaked %q: %q", c.in, c.leak, got)
			}
			if got != c.wantHas {
				t.Fatalf("StripURLUserinfo(%q) = %q, want %q", c.in, got, c.wantHas)
			}
		})
	}
}

func TestRedactorScrubsRegisteredValues(t *testing.T) {
	r := NewRedactor()
	r.AddValue("my-custom-deploy-token-value")
	got := r.Scrub("deploying with my-custom-deploy-token-value now")
	if strings.Contains(got, "my-custom-deploy-token-value") {
		t.Fatalf("registered value leaked: %q", got)
	}
	if !strings.Contains(got, Placeholder) {
		t.Fatalf("expected placeholder: %q", got)
	}
	// Short values are ignored to avoid mangling unrelated text.
	r.AddValue("ab")
	if out := r.Scrub("about"); out != "about" {
		t.Fatalf("short value mangled text: %q", out)
	}
}
