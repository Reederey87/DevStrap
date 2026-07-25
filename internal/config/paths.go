package config

import (
	"os"
	"path/filepath"
)

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
