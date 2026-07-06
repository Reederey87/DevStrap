//go:build darwin

package platform

func Detect() Set {
	return newSet(
		"darwin",
		NativeWatcher{},
		LaunchdManager{},
		SystemKeychain{Platform: "darwin", Target: "keychain"},
		SeatbeltSandbox{},
	)
}
