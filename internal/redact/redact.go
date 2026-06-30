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

// maxLineBytes bounds how much a Writer buffers while waiting for a newline
// (P5-SEC-05). A newline-free stream (a very long single line or a binary blob)
// would otherwise grow the line buffer without limit. When the buffer reaches
// this size without a newline, the partial buffer is scrubbed and flushed so
// memory stays bounded. Confidentiality is preserved — buffered bytes are never
// forwarded until scrubbed.
const maxLineBytes = 1 << 20 // 1 MiB

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

// jsonSecretField matches a JSON object field whose name looks like a secret
// (secret/token/password/private_key/api_key/authorization, case-insensitive)
// and captures the key name so Scrub can mask the value while preserving the
// key (SEC-06). This catches GCP service-account JSON ("private_key":
// "-----BEGIN PRIVATE KEY-----\n...") where the key body is base64 on one line,
// generic Authorization/secret fields a child process may echo, and Snowflake
// config passwords — shapes the bare token-prefix patterns cannot see.
var jsonSecretField = regexp.MustCompile(`(?i)"([A-Za-z0-9_-]*(?:secret|token|password|private[_-]?key|api[_-]?key|authorization)[A-Za-z0-9_-]*)"\s*:\s*"[^"]*"`)

// jsonSecretReplacement preserves the captured key name while masking its
// value (SEC-06), e.g. "private_key":"-----BEGIN..." -> "private_key":"[REDACTED]".
const jsonSecretReplacement = `"$1":"` + Placeholder + `"`

// tokenPatterns matches the shapes of common credentials so that values not
// registered with a Redactor are still scrubbed on a best-effort basis. Order
// matters: the URL-userinfo and JSON-secret-field patterns are special-cased in
// Scrub (they preserve surrounding structure), and jsonSecretField runs before
// the PEM-header pattern so a one-line JSON private_key is fully masked instead
// of leaving its base64 body exposed.
var tokenPatterns = []*regexp.Regexp{
	// URL userinfo: *********************** or scheme://token@host
	regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^/@\s:]+(?::[^/@\s]*)?@`),
	// SEC-06: JSON secret fields (secret/token/password/private_key/api_key/
	// authorization). Special-cased in Scrub to preserve the key name.
	jsonSecretField,
	// GitHub tokens (PAT, OAuth, app, refresh, server-to-server).
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,255}`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,255}`),
	// Slack tokens.
	regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`),
	// AWS access key id.
	regexp.MustCompile(`A(?:KIA|SIA|GPA|IDA|ROA|NPA|NVA|3T)[0-9A-Z]{16}`),
	// OpenAI / generic sk- secret keys (requires a hyphen, so sk_live_ is covered
	// by the dedicated Stripe pattern below).
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{20,}`),
	// SEC-06: Stripe live secret/restricted keys (sk_live_/rk_live_).
	regexp.MustCompile(`(?:sk|rk)_live_[0-9A-Za-z]{20,}`),
	// SEC-06: GitLab personal/project/group access tokens (glpat-).
	regexp.MustCompile(`glpat-[0-9A-Za-z_-]{20,}`),
	// SEC-06: generic Authorization: Bearer <token>.
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._-]{20,}`),
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
		switch p {
		case urlUserinfo:
			s = p.ReplaceAllString(s, "${1}"+Placeholder+"@")
		case jsonSecretField:
			// SEC-06: preserve the JSON key name, mask the value.
			s = p.ReplaceAllString(s, jsonSecretReplacement)
		default:
			s = p.ReplaceAllString(s, Placeholder)
		}
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
// P5-SEC-05: a newline-free run longer than maxLineBytes is scrubbed and
// flushed as a partial line so the buffer cannot grow without bound.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	for {
		data := w.buf.Bytes()
		idx := bytes.IndexByte(data, '\n')
		if idx < 0 {
			// P5-SEC-05: no complete line yet. If the buffer has grown past the
			// cap, flush the oversized partial (scrubbed) so memory stays
			// bounded; otherwise wait for more input.
			if w.buf.Len() > maxLineBytes {
				partial := w.buf.String()
				w.buf.Reset()
				if err := w.emitLine(partial); err != nil {
					return len(p), err
				}
			}
			break
		}
		line := string(data[:idx+1])
		w.buf.Next(idx + 1)
		if err := w.emitLine(line); err != nil {
			return len(p), err
		}
	}
	return len(p), nil
}

// emitLine writes one (possibly newline-terminated) line to the destination,
// honoring the multi-line PEM suppression state. The caller holds w.mu.
func (w *Writer) emitLine(line string) error {
	if w.inPEM {
		if pemEnd.MatchString(line) {
			w.inPEM = false
		}
		return nil // drop body lines inside a PEM block
	}
	if pemBegin.MatchString(line) {
		w.inPEM = true
		_, err := io.WriteString(w.dst, "[REDACTED PRIVATE KEY]\n")
		return err
	}
	_, err := io.WriteString(w.dst, w.scrub(line))
	return err
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
	return w.emitLine(line)
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
