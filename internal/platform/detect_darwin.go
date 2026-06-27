//go:build darwin

package platform

func Detect() Set {
	return newSet(
		"darwin",
		NativeWatcher{},
		UnsupportedServiceManager{Platform: "darwin", Target: "launchd"},
		SystemKeychain{Platform: "darwin", Target: "keychain"},
	)
}
