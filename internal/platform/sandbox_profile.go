package platform

import (
	"strings"
)

// Credential DIRECTORIES at the home root. `.config/gcloud` (GCP refresh
// tokens) and `.azure` (Azure CLI tokens) join the set for P7-SEC-01 — under
// the default guarded policy a compromised child could otherwise read them by
// absolute path and exfiltrate cloud credentials.
var sensitiveHomeDirs = []string{".ssh", ".aws", ".gnupg", ".config/gh", ".config/gcloud", ".azure", ".kube", ".docker"}

// Credential FILES at the home root — the same names the wrapper's
// sensitive-token scanner flags (AGEN-05 alignment, review P3): .netrc
// carries git-https creds, .npmrc/.pypirc registry tokens, .gitconfig
// credential-helper config, and .git-credentials git's plaintext
// credential.helper store (the file .gitconfig merely points at, P7-SEC-01).
var sensitiveHomeFiles = []string{".netrc", ".npmrc", ".pypirc", ".gitconfig", ".git-credentials"}

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
//   - reads of credential-bearing paths are denied when DenySensitiveReads —
//     the caller supplies the deny lists (denyReadDirs as (subpath ...),
//     denyReadFiles as (literal ...)) already resolved for leaf symlinks;
//   - all network access is denied when DenyNetwork.
//
// The credential deny lists are parameters rather than derived from
// spec.UserHome here: Seatbelt matches the kernel-real path, so the darwin
// adapter must resolve leaf symlinks (~/.ssh -> /elsewhere) before this pure
// builder runs. Mirrors the bubblewrap pure-builder / adapter split.
//
// Paths must already be symlink-resolved by the caller: Seatbelt matches the
// kernel-real path, and /tmp -> /private/tmp, TMPDIR -> /private/var/folders.
func sbplProfile(spec SandboxSpec, denyReadDirs, denyReadFiles []string) string {
	var b strings.Builder
	b.WriteString("(version 1)\n")
	b.WriteString("(allow default)\n")

	// Write confinement: deny everywhere, then re-allow the sanctioned roots.
	// Deliberately NOT writable: spec.LogDir — the agent log is written by the
	// PARENT process (the child only inherits pipes), so granting the child
	// LogDir would let it tamper with its own 0600 log and the profile file.
	if spec.ViolationTag == "" {
		b.WriteString("(deny file-write*)\n")
	} else {
		b.WriteString("(deny file-write*\n")
		b.WriteString(sbplWithMessage(spec.ViolationTag))
		b.WriteString(")\n")
	}
	b.WriteString("(allow file-write*\n")
	// WorktreeDir + TmpDir, plus the linked worktree's git storage dirs
	// (objects/refs/logs/per-worktree-admin) so `git commit` works — but NOT
	// the common dir's hooks/config (P7-SANDBOX-01).
	for _, dir := range append([]string{spec.WorktreeDir, spec.TmpDir}, spec.GitDirs...) {
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

	// Read confinement: deny all reads, re-allow the sanctioned roots, and keep
	// a global metadata allow so stat/traversal/symlink-resolution still works
	// (a deliberate, documented path-existence leak — the alternative breaks
	// nearly every tool). The credential deny block below is emitted AFTER this
	// and, since SBPL is last-match-wins, out-ranks the root allows, so a
	// credential path inside an allowed root stays denied. Only emitted when
	// read confinement is requested; the untagged, non-confined profile is
	// byte-identical to before.
	if spec.ReadConfine {
		if spec.ViolationTag == "" {
			b.WriteString("(deny file-read*)\n")
		} else {
			b.WriteString("(deny file-read*\n")
			b.WriteString(sbplWithMessage(spec.ViolationTag))
			b.WriteString(")\n")
		}
		b.WriteString("(allow file-read-metadata)\n")
		b.WriteString("(allow file-read*\n")
		for _, root := range readConfineRoots(spec) {
			b.WriteString("  (subpath " + sbplQuote(root) + ")\n")
		}
		b.WriteString(")\n")
	}

	// A bare "(deny file-read*)" with no filter denies ALL reads, so only emit
	// the block when there is at least one anchor to deny.
	if spec.DenySensitiveReads && (len(denyReadDirs) > 0 || len(denyReadFiles) > 0) {
		b.WriteString("(deny file-read*\n")
		for _, dir := range denyReadDirs {
			b.WriteString("  (subpath " + sbplQuote(dir) + ")\n")
		}
		for _, file := range denyReadFiles {
			b.WriteString("  (literal " + sbplQuote(file) + ")\n")
		}
		b.WriteString(sbplWithMessage(spec.ViolationTag))
		b.WriteString(")\n")
	}

	if spec.DenyNetwork {
		if spec.ViolationTag == "" {
			b.WriteString("(deny network*)\n")
		} else {
			b.WriteString("(deny network*\n")
			b.WriteString(sbplWithMessage(spec.ViolationTag))
			b.WriteString(")\n")
		}
	}

	return b.String()
}

func sbplWithMessage(tag string) string {
	if tag == "" {
		return ""
	}
	return "  (with message " + sbplQuote(tag) + ")\n"
}

// sbplQuote renders a path as an SBPL double-quoted string literal. SBPL is a
// Scheme dialect: backslash and double-quote are the only characters that
// need escaping inside a quoted string.
func sbplQuote(path string) string {
	escaped := strings.ReplaceAll(path, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `"` + escaped + `"`
}
