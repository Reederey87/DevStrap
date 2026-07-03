package cli

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Reederey87/DevStrap/internal/state"
	"github.com/spf13/viper"
)

func TestShouldWarnWorkspaceIDMismatch(t *testing.T) {
	tests := []struct {
		name       string
		role       string
		hubID      string
		pullCursor int64
		hasEvents  bool
		want       bool
	}{
		{
			name:       "joiner r2 cursor zero no events",
			role:       "joiner",
			hubID:      "r2:ws_test",
			pullCursor: 0,
			hasEvents:  false,
			want:       true,
		},
		{
			name:       "founder r2 cursor zero no events",
			role:       "founder",
			hubID:      "r2:ws_test",
			pullCursor: 0,
			hasEvents:  false,
			want:       false,
		},
		{
			name:       "joiner file cursor zero no events",
			role:       "joiner",
			hubID:      "file:/tmp/hub.json",
			pullCursor: 0,
			hasEvents:  false,
			want:       false,
		},
		{
			name:       "joiner r2 cursor advanced no events",
			role:       "joiner",
			hubID:      "r2:ws_test",
			pullCursor: 100,
			hasEvents:  false,
			want:       false,
		},
		{
			name:       "joiner r2 cursor zero has events",
			role:       "joiner",
			hubID:      "r2:ws_test",
			pullCursor: 0,
			hasEvents:  true,
			want:       false,
		},
		{
			name:       "joiner s3 cursor zero no events",
			role:       " joiner ",
			hubID:      "s3:ws_test",
			pullCursor: 0,
			hasEvents:  false,
			want:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldWarnWorkspaceIDMismatch(tt.role, tt.hubID, tt.pullCursor, tt.hasEvents)
			if got != tt.want {
				t.Fatalf("shouldWarnWorkspaceIDMismatch(%q, %q, %d, %v) = %v, want %v", tt.role, tt.hubID, tt.pullCursor, tt.hasEvents, got, tt.want)
			}
		})
	}
}

func TestCheckHubHealthWorkspaceIDRowFileHub(t *testing.T) {
	ctx := context.Background()
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init", "--join"); err != nil {
		t.Fatalf("init --join stderr = %q err = %v", stderr, err)
	}

	store, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	wsID, err := store.WorkspaceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	closeStore(store)

	v := viper.New()
	v.Set("home", home)
	v.Set("root", root)
	v.Set("role", "joiner")
	opts := &options{v: v}

	results := checkHubHealth(ctx, opts, filepath.Join(t.TempDir(), "hub.json"))
	var foundWorkspaceID bool
	for _, result := range results {
		if result.Name == "workspace id" {
			foundWorkspaceID = true
			if result.Status != checkOK || result.Detail != wsID {
				t.Fatalf("workspace id row = %+v, want ok detail %q", result, wsID)
			}
		}
		if result.Name == "workspace id match" {
			t.Fatalf("file-backed hub emitted workspace id match warning: %+v", result)
		}
	}
	if !foundWorkspaceID {
		t.Fatalf("checkHubHealth results = %+v, want workspace id row", results)
	}
}
