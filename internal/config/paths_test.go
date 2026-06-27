package config

import (
	"path/filepath"
	"testing"
)

func TestPathsJoinStateAndLogs(t *testing.T) {
	paths := Paths{Home: filepath.Join(t.TempDir(), ".devstrap"), Root: "/tmp/Code"}
	if got, want := paths.StateDB(), filepath.Join(paths.Home, "state.db"); got != want {
		t.Fatalf("StateDB() = %q, want %q", got, want)
	}
	if got, want := paths.LogDir(), filepath.Join(paths.Home, "logs"); got != want {
		t.Fatalf("LogDir() = %q, want %q", got, want)
	}
	if got, want := paths.KeyDir(), filepath.Join(paths.Home, "keys"); got != want {
		t.Fatalf("KeyDir() = %q, want %q", got, want)
	}
}
