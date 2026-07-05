package platform

// SandboxHelperCommand is the hidden re-exec shim subcommand backing the
// Landlock fallback; exported so the CLI registers the cobra command under
// exactly the name this package renders into the wrapped argv. Landlock
// restricts the calling process and cannot be applied between fork and exec
// by os/exec (golang/go#68595), so the parent re-execs devstrap itself: the
// shim applies the ruleset to its own process, then execve()s the agent argv
// in the same PID — ctx-kill and exit-code semantics are untouched, and the
// ruleset persists across execve.
const SandboxHelperCommand = "sandbox-helper"

// sandboxHelperArgs renders the wrapped argv for the Landlock re-exec shim.
// The spec rides as one JSON argv element: the child env is the sanitized
// childenv allowlist, so env transport would need allowlist changes plus
// post-exec scrubbing, and the spec holds only paths, not secrets. The "--"
// terminator makes dash-prefixed agent argv safe (pflag stops flag parsing
// there), unlike bwrap, which has no terminator and guards argv[0] instead.
func sandboxHelperArgs(self string, specJSON string, argv []string) []string {
	out := []string{self, SandboxHelperCommand, "--spec", specJSON, "--"}
	return append(out, argv...)
}

// landlockLimitations names, per kernel Landlock ABI, every guarantee the
// bubblewrap backend provides that this fallback does not (spec/18 decision:
// additive-allow, so read-denial stays bwrap-only). Kept build-tag-free and
// pure so the degrade contract is unit-tested on every platform.
func landlockLimitations(abi int) []string {
	lims := []string{
		"credential reads are NOT denied (Landlock is additive-allow; read masks stay bubblewrap-only)",
		"no mount/pid namespace: /tmp and /dev/shm stay shared and orphaned grandchildren can outlive the run",
	}
	if abi >= 4 {
		lims = append(lims, "network deny covers TCP bind/connect only (UDP and unix sockets stay open)")
	} else {
		lims = append(lims, "network deny NOT enforced (kernel Landlock ABI < 4)")
	}
	return lims
}
