// Package redact provides value-level secret scrubbing for logs, errors,
// event payloads, and subprocess output.
//
// It complements the key-name-based attribute redaction in internal/logging:
// that layer hides secrets attached under suspicious slog attribute KEYS,
// while this layer catches secret VALUES regardless of the field, string, or
// stream they appear in. The two are defense-in-depth — neither is sufficient
// alone.
//
// The Secret capability type renders as the redaction placeholder through every
// common formatting and serialization path (fmt %v/%s/%#v, encoding/json,
// encoding/text, and slog), exposing the plaintext only through the single
// audited Reveal boundary.
package redact

import (
	"bytes"
	"io"
	"log/slog"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Placeholder is the text substituted for any redacted value.
const Placeholder = "[REDACTED]"

// Secret wraps a sensitive string so it cannot be accidentally logged,
// printed, or serialized in cleartext. Every standard rendering path returns
// Placeholder; the plaintext is reachable only via Reveal.
type Secret struct{ v string }

// New wraps v in a Secret.
func New(v string) Secret { return Secret{v: v} }

// String implements fmt.Stringer.
func (s Secret) String() string { return Placeholder }

// GoString implements fmt.GoStringer so %#v also redacts.
func (s Secret) GoString() string { return Placeholder }

// MarshalText implements encoding.TextMarshaler.
func (s Secret) MarshalText() ([]byte, error) { return []byte(Placeholder), nil }

// MarshalJSON implements json.Marshaler.
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + Placeholder + `"`), nil }

// LogValue implements slog.LogValuer so structured logs redact even when the
// Secret is passed under a benign attribute key.
func (s Secret) LogValue() slog.Value { return slog.StringValue(Placeholder) }

// Reveal returns the underlying plaintext. This is the single audited boundary
// through which secret material leaves the type; keep call sites few and
// obvious.
func (s Secret) Reveal() string { return s.v }

// IsZero reports whether the Secret holds an empty value.
func (s Secret) IsZero() bool { return s.v == "" }

// tokenPatterns matches the shapes of common credentials so that values not
// registered with a Redactor are still scrubbed on a best-effort basis. Order
// does not matter; all patterns are applied.
var tokenPatterns = []*regexp.Regexp{
	// URL userinfo: scheme://user:pass@host or scheme://token@host
	regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^/@\s:]+(?::[^/@\s]*)?@`),
	// GitHub tokens (PAT, OAuth, app, refresh, server-to-server).
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,255}`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,255}`),
	// Slack tokens.
	regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`),
	// AWS access key id.
	regexp.MustCompile(`A(?:KIA|SIA|GPA|IDA|ROA|NPA|NVA|3T)[0-9A-Z]{16}`),
	// OpenAI / generic sk- secret keys.
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{20,}`),
	// Google API key.
	regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`),
	// age secret key.
	regexp.MustCompile(`AGE-SECRET-KEY-1[0-9A-Za-z]{50,}`),
	// Private key PEM header.
	regexp.MustCompile(`-----BEGIN (?:RSA |EC |OPENSSH |DSA |PGP )?PRIVATE KEY-----`),
	// JSON Web Token.
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`),
}

// urlUserinfo is the subexpression that, for the URL userinfo pattern, must be
// preserved (the scheme prefix) while the credentials are dropped.
var urlUserinfo = tokenPatterns[0]

// URL strips userinfo (user:password@ or token@) from a URL-like string while
// preserving the rest. Non-URL input is returned unchanged.
func URL(s string) string {
	if u, err := url.Parse(strings.TrimSpace(s)); err == nil && u.User != nil {
		u.User = url.User(Placeholder)
		return u.String()
	}
	// Fall back to the regex for inputs url.Parse does not treat as having
	// userinfo (e.g. embedded in a larger error string).
	return urlUserinfo.ReplaceAllString(s, "${1}"+Placeholder+"@")
}

// StripURLUserinfo removes credentials from a URL while keeping it valid and
// usable. For http/https the whole userinfo is dropped (it can only be a
// credential). For ssh/git the username is the SSH login name (typically
// "git"), not a secret, so it is preserved and only any password is dropped.
// Non-URL input and URLs without userinfo are returned unchanged.
func StripURLUserinfo(s string) string {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || u.User == nil {
		return s
	}
	switch u.Scheme {
	case "http", "https":
		u.User = nil
	default:
		// ssh, git, etc.: keep the login name, drop any password.
		u.User = url.User(u.User.Username())
	}
	return u.String()
}

// pemEnd matches the END line of a PEM private key block (SECU-04).
var pemEnd = regexp.MustCompile(`-----END (?:RSA |EC |OPENSSH |DSA |PGP )?PRIVATE KEY-----`)

// pemBegin matches the BEGIN line of a PEM private key block.
var pemBegin = regexp.MustCompile(`-----BEGIN (?:RSA |EC |OPENSSH |DSA |PGP )?PRIVATE KEY-----`)

// Scrub applies only the built-in token-shape patterns. Use it when no
// instance-specific secret values are known.
func Scrub(s string) string {
	for _, p := range tokenPatterns {
		if p == urlUserinfo {
			s = p.ReplaceAllString(s, "${1}"+Placeholder+"@")
			continue
		}
		s = p.ReplaceAllString(s, Placeholder)
	}
	return s
}

// Redactor scrubs a set of known secret VALUES (registered via AddValue) in
// addition to the built-in token-shape patterns. It is safe for concurrent
// use.
type Redactor struct {
	mu       sync.Mutex
	values   []string
	replacer *strings.Replacer
}

// NewRedactor returns an empty Redactor.
func NewRedactor() *Redactor { return &Redactor{} }

// AddValue registers a concrete secret value to scrub. Empty and very short
// values are ignored to avoid mangling unrelated text.
func (r *Redactor) AddValue(values ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	changed := false
	for _, v := range values {
		if len(v) < 4 {
			continue
		}
		if containsString(r.values, v) {
			continue
		}
		r.values = append(r.values, v)
		changed = true
	}
	if changed {
		r.replacer = buildReplacer(r.values)
	}
}

// Scrub removes every registered secret value and built-in token shape from s.
func (r *Redactor) Scrub(s string) string {
	r.mu.Lock()
	replacer := r.replacer
	r.mu.Unlock()
	if replacer != nil {
		s = replacer.Replace(s)
	}
	return Scrub(s)
}

// Writer is a line-buffering io.Writer that scrubs secrets from each complete
// line before forwarding it to the destination. It is used to sanitize captured
// subprocess output (e.g. persisted agent logs) so credentials echoed by a tool
// never land on disk in cleartext. Writes are serialized so a single Writer is
// safe to share between an stdout and stderr copier. Call Close to flush any
// trailing partial line.
//
// SECU-04: PEM private key blocks span multiple lines. When a BEGIN PRIVATE
// KEY header is detected, subsequent body lines are suppressed until the
// matching END line, so base64 key material never reaches the destination.
type Writer struct {
	mu    sync.Mutex
	dst   io.Writer
	r     *Redactor
	buf   bytes.Buffer
	inPEM bool // SECU-04: currently inside a multi-line PEM block
}

// NewWriter returns a scrubbing Writer forwarding to dst. If r is nil, only the
// built-in token-shape patterns are applied.
func NewWriter(dst io.Writer, r *Redactor) *Writer {
	return &Writer{dst: dst, r: r}
}

func (w *Writer) scrub(s string) string {
	if w.r != nil {
		return w.r.Scrub(s)
	}
	return Scrub(s)
}

// Write accumulates input and flushes scrubbed complete lines to the
// destination. It always reports len(p) consumed.
// SECU-04: multi-line PEM private key blocks are suppressed across line
// boundaries so base64 body lines never reach the destination.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	for {
		data := w.buf.Bytes()
		idx := bytes.IndexByte(data, '\n')
		if idx < 0 {
			break
		}
		line := string(data[:idx+1])
		w.buf.Next(idx + 1)
		if w.inPEM {
			if pemEnd.MatchString(line) {
				w.inPEM = false
			}
			continue // drop body lines inside a PEM block
		}
		if pemBegin.MatchString(line) {
			w.inPEM = true
			if _, err := io.WriteString(w.dst, "[REDACTED PRIVATE KEY]\n"); err != nil {
				return len(p), err
			}
			continue
		}
		if _, err := io.WriteString(w.dst, w.scrub(line)); err != nil {
			return len(p), err
		}
	}
	return len(p), nil
}

// Close flushes any buffered trailing partial line (scrubbed) to the
// destination. It does not close the destination.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buf.Len() == 0 {
		return nil
	}
	line := w.buf.String()
	w.buf.Reset()
	_, err := io.WriteString(w.dst, w.scrub(line))
	return err
}

func buildReplacer(values []string) *strings.Replacer {
	// Replace longest values first so a value that is a prefix of another does
	// not partially mask it.
	sorted := append([]string(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return len(sorted[i]) > len(sorted[j]) })
	pairs := make([]string, 0, len(sorted)*2)
	for _, v := range sorted {
		pairs = append(pairs, v, Placeholder)
	}
	return strings.NewReplacer(pairs...)
}

func containsString(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}
