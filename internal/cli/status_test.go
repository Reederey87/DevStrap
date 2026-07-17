package cli

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Reederey87/DevStrap/internal/state"
)

// P5-PROD-01: deriveDisplayStatus must branch only on states writers actually
// produce, and the headline "ready" must be reachable (materialized + clean).
func TestDeriveDisplayStatus(t *testing.T) {
	cases := []struct {
		materialization string
		dirty           string
		want            string
	}{
		{"skeleton", "unknown", "skeleton"},
		{"failed", "unknown", "failed"},
		{"materialized-empty", "clean", "empty checkout"},
		{"available", "clean", "ready"}, // the headline readiness state
		{"available", "dirty", "dirty"},
		{"available", "diverged", "dirty"},
		{"available", "unknown", "available"},
		{"available", "", "available"},
	}
	for _, c := range cases {
		if got := deriveDisplayStatus(c.materialization, c.dirty); got != c.want {
			t.Errorf("deriveDisplayStatus(%q,%q) = %q, want %q", c.materialization, c.dirty, got, c.want)
		}
	}
}

// TestStatusAllDevices pins the spec/07 Layer A requirement that
// `status --all-devices` must always render an observed row per project — a
// project with a device_gitstate observation shows "last seen ... ago", and a
// project with ZERO observations still gets an explicit "never synced" row,
// never a silent omission.
func TestStatusAllDevices(t *testing.T) {
	ctx := context.Background()
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init", "--workspace-name", "personal"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}

	store, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertProject(ctx, state.UpsertProjectParams{Path: "work/acme/api", Type: "plain_folder"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertProject(ctx, state.UpsertProjectParams{Path: "work/acme/never", Type: "plain_folder"}); err != nil {
		t.Fatal(err)
	}
	observedHLC := state.HLCFromPhysicalTime(time.Now().Add(-2 * time.Hour))
	if err := store.WithTx(ctx, func(tx *state.Tx) error {
		return tx.UpsertDeviceGitstateTx(ctx, "dev_peer", "work/acme/api", "work/acme/api", state.GitstateParams{
			Branch: "main", HeadSHA: "abc123", DirtyCount: 2, UntrackedCount: 1,
		}, state.Event{ID: "evt_gs_status", HLC: observedHLC})
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "status", "--all-devices")
	if err != nil {
		t.Fatalf("status --all-devices stderr = %q err = %v", stderr, err)
	}
	if !strings.Contains(stdout, "dev_peer") || !strings.Contains(stdout, "last seen") {
		t.Fatalf("status --all-devices human output missing observed device row:\n%s", stdout)
	}
	if !strings.Contains(stdout, "work/acme/never") || !strings.Contains(stdout, "never synced") {
		t.Fatalf("status --all-devices must render an explicit never-synced row, not silently omit the project:\n%s", stdout)
	}

	stdout, stderr, err = executeForTest("--home", home, "--root", root, "--json", "status", "--all-devices")
	if err != nil {
		t.Fatalf("status --all-devices --json stderr = %q err = %v", stderr, err)
	}
	var out []projectGitstateStatus
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("status --all-devices --json is not a bare array: %v\n%s", err, stdout)
	}
	byPath := make(map[string]projectGitstateStatus, len(out))
	for _, p := range out {
		byPath[p.Path] = p
	}
	api, ok := byPath["work/acme/api"]
	if !ok || len(api.Devices) != 1 || api.Devices[0].DeviceID != "dev_peer" || !strings.Contains(api.Devices[0].Observed, "last seen") {
		t.Fatalf("api project gitstate = %+v, want one dev_peer row with an observed age", api)
	}
	never, ok := byPath["work/acme/never"]
	if !ok || len(never.Devices) != 1 || never.Devices[0].Observed != "never synced" {
		t.Fatalf("never-synced project gitstate = %+v, want an explicit never-synced row — spec/07 forbids a silent all-clear", never)
	}
}

// TestGitstateRowsForProject pins the fix for a bug where a single project's
// DeviceGitstateForProject error aborted renderAllDevicesStatus entirely
// (`return err`), blacking out visibility into every other, already
// successfully-read project. gitstateRowsForProject is the per-project
// mapping renderAllDevicesStatus's loop now always appends to `out` for —
// never returning early — so a failure on one project can only ever produce
// a visible "error: ..." row for that project, not lose the rest of the
// render.
func TestGitstateRowsForProject(t *testing.T) {
	now := time.Now()

	if got := gitstateRowsForProject(nil, errFakeGitstateRead, now); len(got) != 1 || got[0].Observed != "error: boom" {
		t.Fatalf("error-path rows = %+v, want one visible error row, not an empty/aborted result", got)
	}

	if got := gitstateRowsForProject(nil, nil, now); len(got) != 1 || got[0].Observed != "never synced" {
		t.Fatalf("zero-row rows = %+v, want one never-synced row", got)
	}

	rows := []state.DeviceGitstate{{
		DeviceID: "dev_peer", Branch: "main", DirtyCount: 2,
		ObservedAtHLC: state.HLCFromPhysicalTime(now.Add(-90 * time.Minute)),
	}}
	got := gitstateRowsForProject(rows, nil, now)
	if len(got) != 1 || got[0].DeviceID != "dev_peer" || !strings.Contains(got[0].Observed, "last seen") {
		t.Fatalf("populated rows = %+v, want one dev_peer row with an observed age", got)
	}
}

var errFakeGitstateRead = errors.New("boom")

// TestStatusAllDevicesPendingWip pins that `status --all-devices` renders a
// compact pending-WIP summary (working-state validation plane Layer B)
// alongside the existing Layer A gitstate columns for a project with pending
// WIP, while a project with none stays completely silent — unlike gitstate's
// forced "never synced" row, an absent WIP section is the correct rendering
// for the normal, healthy zero-pending case.
func TestStatusAllDevicesPendingWip(t *testing.T) {
	ctx := context.Background()
	home := filepath.Join(t.TempDir(), ".devstrap")
	root := filepath.Join(t.TempDir(), "Code")
	if _, stderr, err := executeForTest("--home", home, "--root", root, "init", "--workspace-name", "personal"); err != nil {
		t.Fatalf("init stderr = %q err = %v", stderr, err)
	}

	store, err := state.Open(ctx, filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertProject(ctx, state.UpsertProjectParams{Path: "work/acme/pending", Type: "git_repo"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertProject(ctx, state.UpsertProjectParams{Path: "work/acme/clean", Type: "git_repo"}); err != nil {
		t.Fatal(err)
	}
	observedHLC := state.HLCFromPhysicalTime(time.Now().Add(-30 * time.Minute))
	if err := store.WithTx(ctx, func(tx *state.Tx) error {
		return tx.UpsertDeviceWipTx(ctx, "dev_peer", "work/acme/pending", "work/acme/pending", state.WipParams{
			Ref: "refs/devstrap/wip/dev_peer/work/acme/pending", SHA: "deadbeef", BaseSHA: "cafef00d",
		}, state.Event{ID: "evt_wip_status", HLC: observedHLC})
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, err := executeForTest("--home", home, "--root", root, "status", "--all-devices")
	if err != nil {
		t.Fatalf("status --all-devices stderr = %q err = %v", stderr, err)
	}
	if !strings.Contains(stdout, "pending WIP for work/acme/pending") || !strings.Contains(stdout, "dev_peer") {
		t.Fatalf("status --all-devices missing pending WIP summary:\n%s", stdout)
	}
	if strings.Contains(stdout, "pending WIP for work/acme/clean") {
		t.Fatalf("status --all-devices must stay silent for a project with zero pending WIP, not render an empty/placeholder entry:\n%s", stdout)
	}

	stdout, stderr, err = executeForTest("--home", home, "--root", root, "--json", "status", "--all-devices")
	if err != nil {
		t.Fatalf("status --all-devices --json stderr = %q err = %v", stderr, err)
	}
	var out []projectGitstateStatus
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("status --all-devices --json is not a bare array: %v\n%s", err, stdout)
	}
	byPath := make(map[string]projectGitstateStatus, len(out))
	for _, p := range out {
		byPath[p.Path] = p
	}
	pending, ok := byPath["work/acme/pending"]
	if !ok || len(pending.WIP) != 1 || pending.WIP[0].DeviceID != "dev_peer" {
		t.Fatalf("pending project wip = %+v, want one dev_peer WIP row", pending)
	}
	clean, ok := byPath["work/acme/clean"]
	if !ok || len(clean.WIP) != 0 {
		t.Fatalf("clean project wip = %+v, want no WIP rows, not an empty/placeholder entry", clean)
	}
}
