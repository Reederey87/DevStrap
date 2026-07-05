package platform

import (
	"path/filepath"
	"strings"
)

var sensitiveHomeDirs = []string{".ssh", ".aws", ".gnupg", ".config/gh", ".kube", ".docker"}

// Credential FILES at the home root — the same names the wrapper's
// sensitive-token scanner flags (AGEN-05 alignment, review P3): .netrc
// carries git-https creds, .npmrc/.pypirc registry tokens, .gitconfig
// credential-helper config.
var sensitiveHomeFiles = []string{".netrc", ".npmrc", ".pypirc", ".gitconfig"}

// sbplProfile renders the macOS Seatbelt (SBPL) profile for a SandboxSpec.
// Kept build-tag-free and pure so the profile shape is unit-tested on every
// platform, not just darwin.
//
// Shape: allow-default with targeted denies — the pragmatic pattern for
// wrapping arbitrary user toolchains (a deny-default profile breaks compilers,
// package managers, and Apple-signed helpers on day one). What it enforces:
//
//   - writes are confined to the worktree, the per-run temp dir, and a small
//     set of device nodes (the log dir is deliberately parent-only);
//   - reads of credential-bearing paths are denied when DenySensitiveReads;
//   - all network access is denied when DenyNetwork.
//
// Paths must already be symlink-resolved by the caller: Seatbelt matches the
// kernel-real path, and /tmp -> /private/tmp, TMPDIR -> /private/var/folders.
func sbplProfile(spec SandboxSpec) string {
	var b strings.Builder
	b.WriteString("(version 1)\n")
	b.WriteString("(allow default)\n")

	// Write confinement: deny everywhere, then re-allow the sanctioned roots.
	// Deliberately NOT writable: spec.LogDir — the agent log is written by the
	// PARENT process (the child only inherits pipes), so granting the child
	// LogDir would let it tamper with its own 0600 log and the profile file.
	b.WriteString("(deny file-write*)\n")
	b.WriteString("(allow file-write*\n")
	for _, dir := range []string{spec.WorktreeDir, spec.TmpDir} {
		if dir == "" {
			continue
		}
		b.WriteString("  (subpath " + sbplQuote(dir) + ")\n")
	}
	// Terminal/device plumbing that ordinary tools write to.
	b.WriteString("  (literal \"/dev/null\")\n")
	b.WriteString("  (literal \"/dev/zero\")\n")
	b.WriteString("  (literal \"/dev/tty\")\n")
	b.WriteString("  (literal \"/dev/dtracehelper\")\n")
	b.WriteString("  (regex #\"^/dev/ttys[0-9]+$\")\n")
	b.WriteString(")\n")

	if spec.DenySensitiveReads {
		b.WriteString("(deny file-read*\n")
		if spec.UserHome != "" {
			for _, rel := range sensitiveHomeDirs {
				b.WriteString("  (subpath " + sbplQuote(filepath.Join(spec.UserHome, rel)) + ")\n")
			}
			for _, rel := range sensitiveHomeFiles {
				b.WriteString("  (literal " + sbplQuote(filepath.Join(spec.UserHome, rel)) + ")\n")
			}
		}
		if spec.DevstrapHome != "" {
			b.WriteString("  (subpath " + sbplQuote(filepath.Join(spec.DevstrapHome, "keys")) + ")\n")
		}
		b.WriteString(")\n")
	}

	if spec.DenyNetwork {
		b.WriteString("(deny network*)\n")
	}

	return b.String()
}

// sbplQuote renders a path as an SBPL double-quoted string literal. SBPL is a
// Scheme dialect: backslash and double-quote are the only characters that
// need escaping inside a quoted string.
func sbplQuote(path string) string {
	escaped := strings.ReplaceAll(path, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `"` + escaped + `"`
}
