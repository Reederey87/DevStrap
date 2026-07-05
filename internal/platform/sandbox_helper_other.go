//go:build !linux

package platform

import "fmt"

// ExecSandboxHelper is the Landlock re-exec shim body; Landlock is a Linux
// kernel LSM, so every other platform refuses. The hidden CLI command is
// registered everywhere (build-tag-free, per the goos-guard convention) and
// dispatches here.
func ExecSandboxHelper(SandboxSpec, []string) error {
	return fmt.Errorf("%w: sandbox-helper is a linux-only landlock shim", ErrUnsupported)
}
