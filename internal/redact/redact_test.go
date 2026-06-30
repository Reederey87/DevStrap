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

// TestScrubExtendedTokenShapes (SEC-06): GitLab, Stripe, and Bearer secret
// shapes that the original tokenPatterns omitted are now redacted.
func TestScrubExtendedTokenShapes(t *testing.T) {
	bearerBody := secretLike("dGhpcyBpcyBhIHRva2VuIGZvciB0ZXN0aW5n", "")
	secrets := []string{
		secretLike("glpat-", "abcdefghijklmnopqrstuvwxyz0123456789"),
		secretLike("sk_live_", "abcdefghijklmnopqrstuvwxyz0123456789"),
		secretLike("rk_live_", "abcdefghijklmnopqrstuvwxyz0123456789"),
		"Bearer " + bearerBody,
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

// TestScrubJSONSecretFields (SEC-06): a JSON field whose name looks like a
// secret has its value masked while the key name is preserved. This catches the
// GCP service-account private_key (base64 on one JSON line) that the bare
// PEM-header pattern would leave exposed, plus generic api_key/Authorization/
// password/client_secret fields (incl. Snowflake config passwords).
func TestScrubJSONSecretFields(t *testing.T) {
	gcpSA := `{"type":"service_account","private_key":"-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQ\n-----END PRIVATE KEY-----\n","client_email":"svc@test.iam.gserviceaccount.com"}`
	got := Scrub(gcpSA)
	if strings.Contains(got, "MIIEvQ") {
		t.Fatalf("GCP SA private_key body leaked: %q", got)
	}
	if !strings.Contains(got, `"private_key":"`+Placeholder+`"`) {
		t.Fatalf("GCP SA private_key value not masked (key name should be preserved): %q", got)
	}
	if !strings.Contains(got, `"client_email":"svc@test.iam.gserviceaccount.com"`) {
		t.Fatalf("non-secret field client_email mangled: %q", got)
	}

	cases := []struct {
		name, in, leak string
	}{
		{"api_key", `{"api_key":"` + secretLike("ak_live_", "abcdefghijklmnopqrstuvwxyz0123456789") + `"}`, "abcdefghijklmnopqrstuvwxyz0123456789"},
		{"authorization bearer", `{"Authorization":"Bearer ` + secretLike("eyJhbGciOi", "JIUzI1NiIsInR5cCI6IkpXVCJ9.payload.sig") + `"}`, "JIUzI1NiIsInR5cCI6IkpXVCJ9.payload.sig"},
		{"db_password", `{"db_password":"` + secretLike("pw_", "supersecretpassword1234567890ABCDEF") + `"}`, "supersecretpassword1234567890ABCDEF"},
		{"client_secret", `{"client_secret":"` + secretLike("cs_", "abcdefghijklmnopqrstuvwxyz0123456789AB") + `"}`, "abcdefghijklmnopqrstuvwxyz0123456789AB"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Scrub(c.in)
			if strings.Contains(got, c.leak) {
				t.Fatalf("JSON secret value leaked for %q: %q", c.in, got)
			}
			if !strings.Contains(got, Placeholder) {
				t.Fatalf("JSON secret value not replaced with placeholder: %q", got)
			}
		})
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

func TestWriterSuppressesMultilinePEMBlock(t *testing.T) {
	// SECU-04: multi-line PEM private key blocks must be suppressed so base64
	// key material never reaches the destination.
	pemBody := "MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAkEAQWJjZGVmZ2hpamts" +
		"bW5vcHFyc3R1dnd4eXoxMjM0NTY3ODkwQUIvQWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4"
	input := strings.Join([]string{
		"Starting deploy...",
		"-----BEGIN RSA PRIVATE KEY-----",
		pemBody,
		"-----END RSA PRIVATE KEY-----",
		"Deploy complete.",
		"", // trailing newline
	}, "\n")

	var dst strings.Builder
	w := NewWriter(&dst, nil)
	if _, err := w.Write([]byte(input)); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	out := dst.String()
	if strings.Contains(out, pemBody) {
		t.Fatalf("PEM base64 body leaked to destination: %q", out)
	}
	if strings.Contains(out, "BEGIN RSA PRIVATE KEY") {
		t.Fatalf("PEM BEGIN header leaked to destination: %q", out)
	}
	if strings.Contains(out, "END RSA PRIVATE KEY") {
		t.Fatalf("PEM END header leaked to destination: %q", out)
	}
	if !strings.Contains(out, "[REDACTED PRIVATE KEY]") {
		t.Fatalf("expected [REDACTED PRIVATE KEY] placeholder, got: %q", out)
	}
	if !strings.Contains(out, "Starting deploy...") {
		t.Fatalf("text before PEM block was lost: %q", out)
	}
	if !strings.Contains(out, "Deploy complete.") {
		t.Fatalf("text after PEM block was lost: %q", out)
	}
}
