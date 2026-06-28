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
