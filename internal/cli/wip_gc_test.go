package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dsgit "github.com/Reederey87/DevStrap/internal/git"
	"github.com/Reederey87/DevStrap/internal/state"
)

func TestPlanWipGC(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	ttl := 30 * 24 * time.Hour
	old := state.HLCFromPhysicalTime(now.Add(-ttl - time.Hour))
	young := state.HLCFromPhysicalTime(now.Add(-time.Hour))
	sha := strings.Repeat("a", 40)
	newSHA := strings.Repeat("b", 40)
	row := func(device string, observed int64) state.DeviceWip {
		return state.DeviceWip{DeviceID: device, Path: "work/project", PathKey: "project", SHA: sha, ObservedAtHLC: observed}
	}
	advertised := func(gotSHA string) map[string][]dsgit.RemoteRef {
		return map[string][]dsgit.RemoteRef{"project": {{Ref: wipRefFor("self", "project"), SHA: gotSHA}}}
	}
	tests := []struct {
		name, self, target string
		rows               []state.DeviceWip
		remote             map[string][]dsgit.RemoteRef
		trust              map[string]string
		orphans            map[string]wipOrphanRecord
		wantDelete         bool
		wantReason         string
		wantFirstSeen      bool
	}{
		{"row younger than TTL", "self", "", []state.DeviceWip{row("self", young)}, advertised(sha), nil, nil, false, "younger than TTL", false},
		{"own row past TTL", "self", "", []state.DeviceWip{row("self", old)}, advertised(sha), nil, nil, true, "own ref past TTL", false},
		{"revoked owner past TTL", "other", "", []state.DeviceWip{row("self", old)}, advertised(sha), map[string]string{"self": "revoked"}, nil, true, "revoked device past TTL", false},
		{"lost owner past TTL", "other", "", []state.DeviceWip{row("self", old)}, advertised(sha), map[string]string{"self": "lost"}, nil, true, "lost device past TTL", false},
		{"live peer past TTL", "other", "", []state.DeviceWip{row("self", old)}, advertised(sha), map[string]string{"self": "approved"}, nil, false, "live peer; use wip gc --device self", false},
		{"explicit live peer", "other", "self", []state.DeviceWip{row("self", old)}, advertised(sha), map[string]string{"self": "approved"}, nil, true, "explicit device", false},
		{"owner pushed newer", "self", "", []state.DeviceWip{row("self", old)}, advertised(newSHA), nil, nil, false, "owner pushed newer, unsynced", false},
		{"reapable orphan first sweep", "self", "", nil, advertised(sha), nil, nil, false, "orphan first seen", true},
		{"reapable orphan aged", "self", "", nil, advertised(sha), nil, map[string]wipOrphanRecord{wipRefFor("self", "project"): {SHA: sha, FirstSeen: now.Add(-ttl - time.Hour)}}, true, "orphan past TTL", true},
		{"reapable orphan changed", "self", "", nil, advertised(sha), nil, map[string]wipOrphanRecord{wipRefFor("self", "project"): {SHA: newSHA, FirstSeen: now.Add(-ttl - time.Hour)}}, false, "orphan first seen", true},
		{"unknown orphan", "other", "", nil, advertised(sha), nil, nil, false, "unknown owner; not deleted", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			actions, next := planWipGC(tc.rows, tc.remote, tc.trust, tc.self, tc.target, tc.orphans, nil, ttl, now)
			if len(actions) != 1 {
				t.Fatalf("actions = %+v, want one", actions)
			}
			if actions[0].Delete != tc.wantDelete || actions[0].Reason != tc.wantReason {
				t.Fatalf("action = %+v, want delete=%v reason=%q", actions[0], tc.wantDelete, tc.wantReason)
			}
			_, seen := next[wipRefFor("self", "project")]
			if seen != tc.wantFirstSeen {
				t.Fatalf("orphan retained = %v, want %v (next=%+v)", seen, tc.wantFirstSeen, next)
			}
		})
	}
}

func TestCommitAgeCorroborationBlocksAForgedAge(t *testing.T) {
	dir := t.TempDir()
	runGitTest(t, dir, "init")
	runGitTest(t, dir, "config", "user.name", "DevStrap Test")
	runGitTest(t, dir, "config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("now"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, dir, "add", "file")
	runGitTest(t, dir, "commit", "-m", "fresh WIP object")
	sha := strings.TrimSpace(runGitTest(t, dir, "rev-parse", "HEAD"))

	// The mirror can claim an ancient observation, but the sha-bound object
	// date is fresh and therefore must veto the nominated deletion.
	old, err := commitAgeExceeds(context.Background(), dsgit.NewRunner(), dir, sha, 24*time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if old {
		t.Fatal("fresh object passed age corroboration; forged mirror age would authorize deletion")
	}
	action := wipGCAction{SHA: sha, Delete: true, Reason: "own ref past TTL"}
	if corroborateCommitAge(&action, old) {
		t.Fatal("fresh object corroborated a planner nomination")
	}
	if action.Delete || action.Reason != "object is newer than its mirror record; not deleted" {
		t.Fatalf("corroborated action = %+v, want visible non-delete forged-age reason", action)
	}
}

// TestCommitAgeCorroborationAcceptsAGenuinelyOldObject is the POSITIVE control
// the veto test lacks. Without it, a commitAgeExceeds that never reads the date
// — `return false, nil`, or a comparison against epoch zero — passes the veto
// case and the txtar too, since the txtar runs at --ttl 1ms where every commit
// is older than the TTL. This repo's ledger records "a test named for the
// regression it was meant to prevent that could not fail" as a merge-blocking
// defect class; the veto direction alone is exactly that shape.
//
// The backdated commit also pins the FIELD: committer date (%ct), not author
// date (%at). Committer date is the one a rewrite updates, so it is the honest
// "when did this object come to exist here" answer.
func TestCommitAgeCorroborationAcceptsAGenuinelyOldObject(t *testing.T) {
	dir := t.TempDir()
	runGitTest(t, dir, "init")
	runGitTest(t, dir, "config", "user.name", "DevStrap Test")
	runGitTest(t, dir, "config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, dir, "add", "file")
	// Author date stays fresh while the COMMITTER date is backdated a year, so
	// an implementation reading %at instead of %ct fails this case.
	runGitTestEnv(t, dir, []string{"GIT_COMMITTER_DATE=2020-01-02T03:04:05+00:00"}, "commit", "-m", "aged WIP object")
	sha := strings.TrimSpace(runGitTest(t, dir, "rev-parse", "HEAD"))

	old, err := commitAgeExceeds(context.Background(), dsgit.NewRunner(), dir, sha, 24*time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !old {
		t.Fatal("genuinely old object failed age corroboration; GC could never delete anything")
	}
	action := wipGCAction{SHA: sha, Delete: true, Reason: "own ref past TTL"}
	if !corroborateCommitAge(&action, old) {
		t.Fatal("aged object did not corroborate its planner nomination")
	}
	if !action.Delete {
		t.Fatalf("corroborated action = %+v, want the nomination preserved", action)
	}
}

func runGitTestEnv(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func runGitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// TestPlanWipGCOrphanActionCarriesThePathKey pins the executor's resolution
// contract, which is the one thing the planner's own table cannot see.
//
// The orphan branch only ever sees remote refs keyed by PATH KEY, and
// pathkey.Clean lowercases the key — so for any project whose display path
// carries an uppercase character (`Work/Proj` -> `work/proj`) the two differ.
// The first implementation put the key in the action's Path field and the
// executor resolved projects by DISPLAY path, so every aged orphan on such a
// project silently degraded to "project unavailable; not deleted" forever:
// orphan reaping was dead code, and no test noticed because the planner table
// never crosses into the executor.
func TestPlanWipGCOrphanActionCarriesThePathKey(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	ttl := 30 * 24 * time.Hour
	sha := strings.Repeat("c", 40)
	ref := wipRefFor("self", "work/proj")
	seen := map[string]wipOrphanRecord{ref: {SHA: sha, FirstSeen: now.Add(-ttl - time.Hour)}}

	actions, _ := planWipGC(
		nil,
		map[string][]dsgit.RemoteRef{"work/proj": {{Ref: ref, SHA: sha}}},
		map[string]string{}, "self", "", seen, nil, ttl, now,
	)
	if len(actions) != 1 {
		t.Fatalf("actions = %+v, want exactly one orphan action", actions)
	}
	a := actions[0]
	if !a.Delete {
		t.Fatalf("aged own-orphan action = %+v, want a delete nomination", a)
	}
	// The key is what the executor resolves by; the display path is filled in
	// later and must not be assumed here.
	if a.PathKey != "work/proj" {
		t.Fatalf("orphan action PathKey = %q, want the path key the remote was enumerated under", a.PathKey)
	}
}

// TestPlanWipGCDistinguishesUncheckedFromOutdated covers the three states the
// planner used to collapse into a single "owner pushed newer, unsynced" reason.
// This is deletion-forensics output for a data-destroying command: it must not
// assert something about the owner when the remote was never read at all.
func TestPlanWipGCDistinguishesUncheckedFromOutdated(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	ttl := 30 * 24 * time.Hour
	aged := state.HLCFromPhysicalTime(now.Add(-ttl - time.Hour))
	sha := strings.Repeat("a", 40)
	rows := []state.DeviceWip{{DeviceID: "self", Path: "work/project", PathKey: "project", SHA: sha, ObservedAtHLC: aged}}

	for _, tc := range []struct {
		name   string
		remote map[string][]dsgit.RemoteRef
		want   string
	}{
		{"enumeration failed", map[string][]dsgit.RemoteRef{}, "remote enumeration unavailable; not checked"},
		{"ref absent on origin", map[string][]dsgit.RemoteRef{"project": {}}, "ref absent on origin; mirror row is stale"},
		{"owner pushed newer", map[string][]dsgit.RemoteRef{"project": {{Ref: wipRefFor("self", "project"), SHA: strings.Repeat("b", 40)}}}, "owner pushed newer, unsynced"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			actions, _ := planWipGC(rows, tc.remote, map[string]string{}, "self", "", nil, nil, ttl, now)
			if len(actions) == 0 {
				t.Fatal("no actions")
			}
			if actions[0].Delete {
				t.Fatalf("action = %+v, want no deletion", actions[0])
			}
			if actions[0].Reason != tc.want {
				t.Fatalf("reason = %q, want %q", actions[0].Reason, tc.want)
			}
		})
	}
}

// TestPlanWipGCKeepsUnvisitedOrphanRecords pins that a scoped or partially
// failing sweep does not reset every other orphan's first-seen clock. Clobbering
// the map is retention-safe but defers reaping indefinitely under alternating
// scoped runs, and contradicts spec/12's stated pruning rule.
func TestPlanWipGCKeepsUnvisitedOrphanRecords(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	ttl := 30 * 24 * time.Hour
	elsewhere := wipRefFor("self", "other/proj")
	seen := map[string]wipOrphanRecord{elsewhere: {SHA: strings.Repeat("d", 40), FirstSeen: now.Add(-ttl / 2)}}

	// Sweep visits only "project"; the record for "other/proj" must survive.
	_, next := planWipGC(nil, map[string][]dsgit.RemoteRef{"project": {}}, map[string]string{}, "self", "", seen, nil, ttl, now)
	got, ok := next[elsewhere]
	if !ok {
		t.Fatal("a scoped sweep dropped an unvisited project's orphan record; its TTL would restart from zero")
	}
	if !got.FirstSeen.Equal(seen[elsewhere].FirstSeen) {
		t.Fatalf("first-seen = %v, want the original %v", got.FirstSeen, seen[elsewhere].FirstSeen)
	}
}

// TestPlanWipGCRefusesToReapShaAgnosticallyHiddenRef pins P9-WIP-06, the
// decision this repo had to make rather than patch around.
//
// A sha-agnostic drop tombstone — published when the remote ref was already
// gone — deliberately outranks any sha guess so a phantom row clears. Under HLC
// skew that same rule buries a genuinely LATER push: the row is hidden on every
// device including the owner's, and the still-live ref then looks like an
// unowned orphan the GC should reap. Reaping it converts a recoverable
// visibility bug into permanent loss of the user's recovery data.
//
// Hiding is recoverable — `wip fetch --device <id>` derives the ref canonically
// and ignores the mirror entirely. Deletion is not. Where the two conflict a
// RECOVERY plane must prefer the recoverable outcome, so the GC refuses and
// says why.
func TestPlanWipGCRefusesToReapShaAgnosticallyHiddenRef(t *testing.T) {
	const self = "dev_self"
	ref := wipRefFor(self, "work/proj")
	now := time.Now()
	ttl := time.Hour
	// Long past TTL: without the guard this is a confident delete nomination.
	seen := map[string]wipOrphanRecord{ref: {SHA: "newsha", FirstSeen: now.Add(-72 * time.Hour)}}
	remote := map[string][]dsgit.RemoteRef{"work/proj": {{Ref: ref, SHA: "newsha"}}}

	// Control: with no tombstone hiding it, the aged orphan IS reaped.
	actions, _ := planWipGC(nil, remote, map[string]string{}, self, "", seen, nil, ttl, now)
	if len(actions) != 1 || !actions[0].Delete {
		t.Fatalf("control: aged own-orphan should be nominated for delete; got %+v", actions)
	}

	// With a sha-agnostic tombstone for that exact ref, it must NOT be reaped.
	actions, _ = planWipGC(nil, remote, map[string]string{}, self, "", seen, map[string]bool{ref: true}, ttl, now)
	if len(actions) != 1 {
		t.Fatalf("actions = %+v, want exactly one action", actions)
	}
	if actions[0].Delete {
		t.Fatalf("a ref hidden by a sha-agnostic tombstone must NEVER be deleted — it may be a newer push, "+
			"not an orphan, and deletion is the one outcome that is not recoverable; got %+v", actions[0])
	}
	if !strings.Contains(actions[0].Reason, "sha-agnostic") || !strings.Contains(actions[0].Reason, "wip fetch") {
		t.Fatalf("the refusal must say why and name the recovery path; got %q", actions[0].Reason)
	}
}
