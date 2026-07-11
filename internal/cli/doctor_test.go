package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Reederey87/DevStrap/internal/platform"
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
		{
			name:       "joiner git cursor zero no events",
			role:       "joiner",
			hubID:      "git:ws_test",
			pullCursor: 0,
			hasEvents:  false,
			want:       true,
		},
		{
			name:       "founder git cursor zero no events",
			role:       "founder",
			hubID:      "git:ws_test",
			pullCursor: 0,
			hasEvents:  false,
			want:       false,
		},
		{
			name:       "joiner git cursor advanced no events",
			role:       "joiner",
			hubID:      "git:ws_test",
			pullCursor: 100,
			hasEvents:  false,
			want:       false,
		},
		{
			name:       "joiner folder cursor zero no events",
			role:       "joiner",
			hubID:      "folder:ws_test",
			pullCursor: 0,
			hasEvents:  false,
			want:       true,
		},
		{
			name:       "founder folder cursor zero no events",
			role:       "founder",
			hubID:      "folder:ws_test",
			pullCursor: 0,
			hasEvents:  false,
			want:       false,
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

func TestDoctorWarnsWhenServiceInstalledButStopped(t *testing.T) {
	f := &fakeServiceManager{
		labelVal:  "fake.run-loop",
		statusVal: platform.ServiceStatus{Installed: true, Running: false, Detail: "not loaded", UnitPath: "/x/fake.plist"},
	}
	withFakeService(t, f)

	v := viper.New()
	v.Set("home", t.TempDir())
	opts := &options{v: v}

	results := checkService(context.Background(), opts)
	if len(results) != 1 {
		t.Fatalf("checkService results = %+v, want exactly one", results)
	}
	got := results[0]
	if got.Name != "run-loop service" || got.Status != checkWarn {
		t.Fatalf("service check = %+v, want a warning row", got)
	}
	if !strings.Contains(got.Remedy, "journalctl --user -u fake.run-loop") {
		t.Errorf("remedy = %q, want the inspection hint", got.Remedy)
	}
}

func TestDoctorWarnsWhenServiceExecPathMissing(t *testing.T) {
	f := &fakeServiceManager{statusVal: platform.ServiceStatus{
		Installed:       true,
		Running:         false,
		ExecPath:        "/opt/homebrew/Cellar/devstrap/old/bin/devstrap",
		ExecPathMissing: true,
	}}
	withFakeService(t, f)

	v := viper.New()
	v.Set("home", t.TempDir())
	results := checkService(context.Background(), &options{v: v})
	if len(results) != 1 || results[0].Status != checkWarn {
		t.Fatalf("checkService results = %+v, want one warning", results)
	}
	if !strings.Contains(results[0].Detail, f.statusVal.ExecPath) {
		t.Errorf("detail = %q, want missing path", results[0].Detail)
	}
	wantRemedy := "re-run devstrap service install (the installed unit points at a binary that no longer exists — e.g. after a brew upgrade)"
	if results[0].Remedy != wantRemedy {
		t.Errorf("remedy = %q, want %q", results[0].Remedy, wantRemedy)
	}
}
