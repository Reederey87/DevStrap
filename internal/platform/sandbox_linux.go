//go:build linux

package platform

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// BubblewrapSandbox confines agent children with bubblewrap (AGEN-03 /
// P4-GIT-03 Linux slice): an unprivileged user-namespace mount sandbox that
// ships in every major distro. Availability is probed, not stat'ed, because
// distro policy (Ubuntu 23.10+/24.04 AppArmor userns restriction, container
// seccomp) routinely leaves bwrap installed but unable to create namespaces —
// on such hosts `auto` mode degrades to a loud warning and `require` refuses
// to run.
type BubblewrapSandbox struct{}

func (BubblewrapSandbox) Name() string { return "bubblewrap" }

type bwrapProbeResult struct {
	path          string
	disableUserns bool
}

var probeBwrap = sync.OnceValues(func() (bwrapProbeResult, error) {
	path, err := findBwrap()
	if err != nil {
		return bwrapProbeResult{}, err
	}
	// Ubuntu 23.10+/24.04 AppArmor
	// kernel.apparmor_restrict_unprivileged_userns and container seccomp make
	// bwrap routinely present-but-broken, so Available probes instead of
	// stat'ing.
	firstStderr, firstErr := runBwrapProbe(path, true)
	if firstErr == nil {
		return bwrapProbeResult{path: path, disableUserns: true}, nil
	}
	secondStderr, secondErr := runBwrapProbe(path, false)
	if secondErr == nil {
		return bwrapProbeResult{path: path, disableUserns: false}, nil
	}
	// Prefer the compatible-mode probe's stderr: on bwrap < 0.8 the first
	// attempt fails with an unknown --disable-userns option, which would mask
	// the real denial (e.g. the AppArmor userns restriction) in the message
	// that feeds the --sandbox auto degrade warning.
	stderr := strings.TrimSpace(secondStderr)
	if stderr == "" {
		stderr = strings.TrimSpace(firstStderr)
	}
	if stderr == "" {
		stderr = secondErr.Error()
	}
	return bwrapProbeResult{}, fmt.Errorf("%w: bwrap probe failed: %s", ErrUnsupported, stderr)
})

func findBwrap() (string, error) {
	if info, err := os.Stat("/usr/bin/bwrap"); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
		return "/usr/bin/bwrap", nil
	}
	path, err := exec.LookPath("bwrap")
	if err != nil {
		return "", fmt.Errorf("%w: bwrap not found: %w", ErrUnsupported, err)
	}
	return path, nil
}

func runBwrapProbe(path string, disableUserns bool) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	args := []string{"--unshare-user"}
	if disableUserns {
		args = append(args, "--disable-userns")
	}
	args = append(args, "--unshare-pid", "--ro-bind", "/", "/", "--proc", "/proc", "--dev", "/dev", "true")
	cmd := exec.CommandContext(ctx, path, args...) //nolint:gosec // path comes from fixed executable probe
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return stderr.String(), err
}

func (s BubblewrapSandbox) Available() error {
	_, err := probeBwrap()
	return err
}

func (s BubblewrapSandbox) Command(_ context.Context, spec SandboxSpec, argv []string) ([]string, func(), error) {
	if len(argv) == 0 {
		return nil, func() {}, fmt.Errorf("bubblewrap: empty argv")
	}
	// bwrap(1) documents no -- terminator, so a dash argv[0] would parse as
	// another bwrap option instead of the child command.
	if strings.HasPrefix(argv[0], "-") {
		return nil, func() {}, fmt.Errorf("bubblewrap: argv[0] must not begin with '-'")
	}
	res, err := probeBwrap()
	if err != nil {
		return nil, func() {}, err
	}
	resolved, err := resolveSandboxSpecPaths(spec)
	if err != nil {
		return nil, func() {}, err
	}
	dirs, files := bwrapSensitivePaths(resolved)
	dirs = existingRealPaths(dirs)
	files = existingRealPaths(files)
	wrapped := append([]string{res.path}, append(bwrapArgs(resolved, dirs, files, bwrapOptions{DisableUserns: res.disableUserns}), argv...)...)
	// No profile file exists, so cleanup is a no-op; the Sandbox contract
	// explicitly permits a safe no-op cleanup.
	return wrapped, func() {}, nil
}

func existingRealPaths(paths []string) []string {
	var out []string
	for _, path := range paths {
		// Mount over the REAL target: mounting over a symlink lands on its
		// target, and ~/.ssh -> elsewhere must mask elsewhere.
		real, err := filepath.EvalSymlinks(path)
		if err == nil {
			out = append(out, real)
			continue
		}
		// A genuinely absent credential path has nothing to mask, and bwrap
		// would fail with "Can't mkdir" if asked to mount over a missing dest
		// under the read-only root, so drop it. But EvalSymlinks also fails on
		// permission-denied, symlink loops, and I/O errors — for a mask that
		// backs DenySensitiveReads, silently dropping any of THOSE would leave
		// the credential path readable. Fail closed instead: keep the literal
		// path (bwrap resolves the mount dest itself, so masking the symlink
		// still masks its target; if the dest truly cannot be mounted the run
		// errors rather than proceeding with the credential exposed).
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		out = append(out, path)
	}
	return out
}
