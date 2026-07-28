//go:build darwin || linux

package config

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestValidateSocketPathRejectsOverLongHome(t *testing.T) {
	normal := Paths{Home: filepath.Join(string(filepath.Separator), "tmp", "devstrap")}
	if err := normal.ValidateSocketPath(); err != nil {
		t.Fatalf("normal home rejected: %v", err)
	}

	overlong := Paths{Home: string(filepath.Separator) + strings.Repeat("h", maxUnixSocketPath)}
	socket := overlong.SocketPath()
	err := overlong.ValidateSocketPath()
	if err == nil {
		t.Fatal("over-long socket path accepted")
	}
	if !strings.Contains(err.Error(), strconv.Itoa(len(socket))) {
		t.Fatalf("error = %q, want actual length %d", err, len(socket))
	}
}

func TestSocketPathLimitMatchesRealBind(t *testing.T) {
	atLimit := socketPathOfLength(t, maxUnixSocketPath)
	listener, err := net.Listen("unix", atLimit)
	if err != nil {
		t.Fatalf("net.Listen at %d-byte limit: %v", maxUnixSocketPath, err)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("close at-limit listener: %v", err)
	}

	overLimit := socketPathOfLength(t, maxUnixSocketPath+1)
	listener, err = net.Listen("unix", overLimit)
	assertOverPortableLimitBind(t, listener, err)
}

func socketPathOfLength(t *testing.T, length int) string {
	t.Helper()
	base := filepath.Join("/tmp", "dsp")
	dirLength := length - len(filepath.Join(base, "s")) + len(base)
	if dirLength <= len(base) {
		t.Fatalf("cannot construct %d-byte socket path under %s", length, base)
	}
	dir := base + strings.Repeat("x", dirLength-len(base))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir exact-length socket dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s")
	if len(socket) != length {
		t.Fatalf("constructed socket length = %d, want %d", len(socket), length)
	}
	return socket
}
