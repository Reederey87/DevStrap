//go:build linux

package config

import (
	"net"
	"testing"
)

func assertOverPortableLimitBind(t *testing.T, listener net.Listener, err error) {
	t.Helper()
	// Linux has four more bytes of kernel headroom. The product deliberately
	// rejects this path anyway so homes remain portable to darwin.
	if err != nil {
		t.Fatalf("linux kernel unexpectedly rejected %d-byte path: %v", maxUnixSocketPath+1, err)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("close linux over-darwin-limit listener: %v", err)
	}
}
