package platform

import "runtime"

type RuntimeInfo struct {
	OS   string
	Arch string
}

func Runtime() RuntimeInfo {
	return RuntimeInfo{OS: runtime.GOOS, Arch: runtime.GOARCH}
}
