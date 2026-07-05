//go:build darwin

package platform

import (
	"context"
	"fmt"
	"os/exec"
	"time"
)

// CollectViolations implements SandboxViolationReporter for Seatbelt: it runs
// one post-run `log show` query for the run's tag and parses the Seatbelt deny
// lines it finds. Best-effort — the caller treats any error as non-fatal.
func (s SeatbeltSandbox) CollectViolations(ctx context.Context, tag string, since time.Time) ([]SandboxViolation, error) {
	if tag == "" {
		return nil, nil
	}
	start := since.Add(-2 * time.Second).Format("2006-01-02 15:04:05")
	// Fixed absolute binary; the only variable inputs are the run's own
	// generated tag (devstrap-sb-<runID>, no shell metacharacters) and a
	// formatted timestamp, embedded as `log show` predicate/flag arguments with
	// no shell involved.
	cmd := exec.CommandContext(ctx, "/usr/bin/log", "show", "--style", "syslog", //nolint:gosec // fixed /usr/bin/log; args are a controlled run tag + timestamp, no shell.
		"--predicate", `eventMessage CONTAINS "`+tag+`"`, "--start", start)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("log show: %w", err)
	}
	return parseSeatbeltDenials(string(out)), nil
}
