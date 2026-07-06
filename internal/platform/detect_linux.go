//go:build linux

package platform

func Detect() Set {
	return newSet(
		"linux",
		NativeWatcher{},
		SystemdUserManager{},
		SystemKeychain{Platform: "linux", Target: "secret-service"},
		// bwrap-then-landlock chooser (P4-GIT-03 slice 3); probes stay lazy so
		// Detect itself never launches a subprocess.
		LinuxSandbox{},
	)
}
