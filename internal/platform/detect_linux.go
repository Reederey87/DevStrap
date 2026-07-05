//go:build linux

package platform

func Detect() Set {
	return newSet(
		"linux",
		NativeWatcher{},
		UnsupportedServiceManager{Platform: "linux", Target: "systemd-user"},
		SystemKeychain{Platform: "linux", Target: "secret-service"},
		UnsupportedSandbox{Platform: "linux"},
	)
}
