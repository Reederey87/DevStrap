package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// maxUnixSocketPath is the longest usable Unix socket path. sockaddr_un.sun_path
// is a 104-byte array on darwin (108 on Linux) and the kernel needs room for the
// terminating NUL, so the usable maximum is one less. We apply darwin's tighter
// bound everywhere: a workspace that works on a Mac must work on Linux, and the
// reverse surprise is worse than 4 bytes of headroom.
const maxUnixSocketPath = 103

type Paths struct {
	Home string
	Root string
}

func DefaultPaths() (Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Paths{}, err
	}
	return Paths{
		Home: filepath.Join(home, ".devstrap"),
		Root: filepath.Join(home, "Code"),
	}, nil
}

func (p Paths) StateDB() string {
	return filepath.Join(p.Home, "state.db")
}

func (p Paths) LogDir() string {
	return filepath.Join(p.Home, "logs")
}

func (p Paths) KeyDir() string {
	return filepath.Join(p.Home, "keys")
}

// SocketPath is the local daemon's Unix domain socket (spec/13). It lives
// inside Home rather than a system runtime directory so it inherits the same
// per-user directory that already guards state.db and the key store.
func (p Paths) SocketPath() string {
	return filepath.Join(p.Home, "devstrapd.sock")
}

// ValidateSocketPath reports whether the daemon socket path can actually be
// bound. Callers should invoke it BEFORE net.Listen (which fails with a bare
// EINVAL) and before building a client (which cannot classify that EINVAL).
func (p Paths) ValidateSocketPath() error {
	socket := p.SocketPath()
	if len(socket) > maxUnixSocketPath {
		return fmt.Errorf(
			"daemon socket path is %d bytes, over DevStrap's %d-byte portable limit: %s\n"+
				"choose a shorter state home, e.g. --home ~/.devstrap",
			len(socket), maxUnixSocketPath, socket)
	}
	return nil
}
