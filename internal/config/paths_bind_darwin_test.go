//go:build darwin

package config

import (
	"net"
	"testing"
)

func assertOverPortableLimitBind(t *testing.T, listener net.Listener, err error) {
	t.Helper()
	if err == nil {
		_ = listener.Close()
		t.Fatalf("net.Listen accepted %d-byte path on darwin", maxUnixSocketPath+1)
	}
}
