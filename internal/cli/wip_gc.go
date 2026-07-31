package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	dsgit "github.com/Reederey87/DevStrap/internal/git"
	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
	"github.com/spf13/cobra"
)

const (
	defaultWipGCTTL = 720 * time.Hour
	wipGCOrphansKey = "wip_gc_orphans"
)

type wipOrphanRecord struct {
	SHA       string    `json:"sha"`
	FirstSeen time.Time `json:"first_seen"`
}

type wipGCAction struct {
	Path string `json:"path"`
	// PathKey is how the executor resolves the project. Path is the DISPLAY
	// path and is for humans/JSON only: pathkey.Clean lowercases the key, so on
	// any project whose display path carries an uppercase character the two
	// differ, and resolving by Path silently misses. The orphan branch cannot
	// know the display path at all (it only sees remote refs keyed by path
	// key), so it leaves Path empty for the executor to fill in.
	PathKey  string `json:"-"`
	DeviceID string `json:"device_id"`
	Ref      string `json:"ref"`
	SHA      string `json:"sha"`
	Reason   string `json:"reason"`
	Delete   bool   `json:"delete"`
}

// planWipGC is a pure candidate filter, not deletion authority.
//
// The apparent premise that unverified WIP events drive deletion is false
// after enrollment: verifyEventSignature gates every non-local event when any
// approved/revoked/lost device exists. Promoting repo.wip.pushed/dropped into
// mustVerifyEvent would only alter the single-device bootstrap window, where
// there is no peer to sweep. Residual forged/backdated events can still age a
// mirror (a revoked signer can harm itself; a compromised approved signer can
// amplify its already-accepted fleet revoke power), so mirror state only
// NOMINATES candidates. The executor must corroborate the advertised remote
// sha, fetch that exact object, require its sha-bound committer date to exceed
// the TTL, and delete with a lease on the same sha.
//
// There is deliberately no ack-floor gate: one dead approved device would
// otherwise block the cleanup intended to handle dead devices forever. The
// per-ref corroboration and compare-and-delete lease provide safety instead.
func planWipGC(
	rows []state.DeviceWip,
	remote map[string][]dsgit.RemoteRef,
	trust map[string]string,
	selfID string,
	targetDevice string,
	orphans map[string]wipOrphanRecord,
	ttl time.Duration,
	now time.Time,
) ([]wipGCAction, map[string]wipOrphanRecord) {
	var actions []wipGCAction
	// Start from the EXISTING quarantine rather than an empty map. `next`
	// records only orphans of remotes visited this run, so a project-scoped
	// sweep — or one whose ls-remote failed transiently — used to drop every
	// other orphan's first-seen clock and restart its TTL from zero, deferring
	// reaping indefinitely under alternating scoped runs. Entries for visited
	// path keys are replaced below; the rest carry forward untouched.
	next := make(map[string]wipOrphanRecord, len(orphans))
	for ref, rec := range orphans {
		next[ref] = rec
	}
	for pathKey := range remote {
		for ref := range next {
			if owner, ok := wipOwnerFromRef(ref, pathKey); ok && owner != "" {
				delete(next, ref)
			}
		}
	}
	liveRefs := make(map[string]state.DeviceWip)
	for _, row := range rows {
		liveRefs[wipRefFor(row.DeviceID, row.PathKey)] = row
		advertised := ""
		for _, rr := range remote[row.PathKey] {
			if rr.Ref == wipRefFor(row.DeviceID, row.PathKey) {
				advertised = rr.SHA
				break
			}
		}
		a := wipGCAction{Path: row.Path, PathKey: row.PathKey, DeviceID: row.DeviceID, Ref: wipRefFor(row.DeviceID, row.PathKey), SHA: row.SHA}
		if now.Sub(state.HLCPhysicalTime(row.ObservedAtHLC)) <= ttl {
			a.Reason = "younger than TTL"
		} else if _, enumerated := remote[row.PathKey]; !enumerated {
			// Distinguish the three states this branch used to collapse into
			// "owner pushed newer". This is deletion-forensics output for a
			// data-destroying command, so it must not assert something about
			// the owner when the remote was never successfully read.
			a.Reason = "remote enumeration unavailable; not checked"
		} else if advertised == "" {
			a.Reason = "ref absent on origin; mirror row is stale"
		} else if advertised != row.SHA {
			a.Reason = "owner pushed newer, unsynced"
		} else if targetDevice != "" && row.DeviceID == targetDevice {
			a.Delete, a.Reason = true, "explicit device"
		} else if row.DeviceID == selfID {
			a.Delete, a.Reason = true, "own ref past TTL"
		} else if trust[row.DeviceID] == "revoked" || trust[row.DeviceID] == "lost" {
			a.Delete, a.Reason = true, trust[row.DeviceID]+" device past TTL"
		} else {
			a.Reason = fmt.Sprintf("live peer; use wip gc --device %s", row.DeviceID)
		}
		actions = append(actions, a)
	}
	for pathKey, refs := range remote {
		for _, rr := range refs {
			if _, ok := liveRefs[rr.Ref]; ok {
				continue
			}
			deviceID, ok := wipOwnerFromRef(rr.Ref, pathKey)
			a := wipGCAction{PathKey: pathKey, DeviceID: deviceID, Ref: rr.Ref, SHA: rr.SHA}
			reapable := ok && (deviceID == selfID || trust[deviceID] == "revoked" || trust[deviceID] == "lost" ||
				(targetDevice != "" && deviceID == targetDevice))
			if !reapable {
				// Permanent residual: separate DevStrap workspaces may share an
				// origin. Unknown/live-peer orphans could be another workspace's
				// recovery data, so automatic deletion is unrecoverably unsafe.
				a.Reason = "unknown owner; not deleted"
				actions = append(actions, a)
				continue
			}
			rec, seen := orphans[rr.Ref]
			if !seen || rec.SHA != rr.SHA {
				rec = wipOrphanRecord{SHA: rr.SHA, FirstSeen: now}
				a.Reason = "orphan first seen"
			} else if now.Sub(rec.FirstSeen) > ttl {
				a.Delete, a.Reason = true, "orphan past TTL"
			} else {
				a.Reason = "orphan awaiting TTL"
			}
			next[rr.Ref] = rec
			actions = append(actions, a)
		}
	}
	return actions, next
}

func wipOwnerFromRef(ref, pathKey string) (string, bool) {
	const prefix = "refs/devstrap/wip/"
	suffix := "/" + pathKey
	if !strings.HasPrefix(ref, prefix) || !strings.HasSuffix(ref, suffix) {
		return "", false
	}
	deviceID := strings.TrimSuffix(strings.TrimPrefix(ref, prefix), suffix)
	if deviceID == "" || strings.Contains(deviceID, "/") {
		return "", false
	}
	return deviceID, true
}

// commitAgeExceeds reports whether the commit object's OWN committer date is
// older than ttl. Independent of the event log by construction: the date is
// bound into the sha, and the delete is leased to that same sha.
func commitAgeExceeds(ctx context.Context, r dsgit.Runner, dir, sha string, ttl time.Duration, now time.Time) (bool, error) {
	out, err := r.Run(ctx, dir, "log", "-1", "--format=%ct", sha)
	if err != nil {
		return false, err
	}
	seconds, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return false, fmt.Errorf("parse WIP commit date: %w", err)
	}
	return now.Sub(time.Unix(seconds, 0)) > ttl, nil
}

func corroborateCommitAge(action *wipGCAction, old bool) bool {
	if old {
		return true
	}
	action.Delete = false
	action.Reason = "object is newer than its mirror record; not deleted"
	return false
}

// dropWipRef is shared by explicit drop and GC. It preserves the exact leased
// delete, already-gone disambiguation, event emission, and local tombstone.
func dropWipRef(ctx context.Context, store *state.Store, r dsgit.Runner, project state.ProjectStatus, deviceID, expectedSHA string) error {
	ref := wipRefFor(deviceID, project.PathKey)
	// COMPARE-AND-DELETE against the sha the synced record promised. The owner
	// may have force-pushed a newer snapshot whose event has not arrived.
	// leasedSHA is the sha this delete proved it removed. Already-gone leases
	// nothing and publishes the sha-agnostic empty tombstone.
	// P9-WIP-01, defense in depth: refuse rather than issue an unleased delete.
	// DeleteRef drops --force-with-lease when expectedSHA is empty, so an empty
	// mirror sha would unconditionally destroy whatever the remote holds — the
	// exact loss the compare-and-delete exists to prevent. Apply-time validation
	// now keeps empty shas out of the mirror; this guard means a row that
	// predates that validation, or arrives by any future path, still cannot
	// trigger an unleased delete.
	if strings.TrimSpace(expectedSHA) == "" {
		return appError{code: exitConflict, err: fmt.Errorf("refusing to drop %s: the local record carries no sha, so the delete could not be leased and would destroy any newer recovery snapshot; run `devstrap sync` to refresh the record, then retry", ref)}
	}
	leasedSHA := expectedSHA
	if err := r.DeleteRef(ctx, project.LocalPath, "origin", ref, expectedSHA); err != nil {
		if !errors.Is(err, dsgit.ErrNonFastForward) {
			return appError{code: exitGit, err: err}
		}
		got, lsErr := r.LsRemoteRef(ctx, project.LocalPath, "origin", ref)
		if lsErr != nil && !errors.Is(lsErr, dsgit.ErrBranchNotFound) {
			return appError{code: exitGit, err: lsErr}
		}
		if lsErr == nil {
			return appError{code: exitGit, err: fmt.Errorf(
				"remote WIP ref for device %s has moved past the last synced record (expected %s, found %s); run `devstrap sync` to pull the newer record, then retry",
				deviceID, shortSHA(expectedSHA), shortSHA(got))}
		}
		leasedSHA = ""
	}
	payload := dssync.WipDroppedPayload{Path: project.Path, DeviceID: deviceID, SHA: leasedSHA}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return store.WithTx(ctx, func(tx *state.Tx) error {
		// Mirror the emitting device's event in the same transaction because
		// its re-delivered event is deduplicated.
		ev, err := store.InsertLocalEventTx(ctx, tx, dssync.NewWipDroppedEvent(string(raw)))
		if err != nil {
			return err
		}
		return tx.TombstoneDeviceWipTx(ctx, deviceID, project.PathKey, project.Path, leasedSHA, ev)
	})
}

type wipGCWarning struct {
	Path    string `json:"path,omitempty"`
	Message string `json:"message"`
}

type wipGCResult struct {
	TTL      string         `json:"ttl"`
	DryRun   bool           `json:"dry_run"`
	Actions  []wipGCAction  `json:"actions,omitempty"`
	Warnings []wipGCWarning `json:"warnings,omitempty"`
}

type wipGCOptions struct {
	TargetDevice string
	ProjectPath  string
	TTL          time.Duration
	DryRun       bool
}

func sweepWipRefs(ctx context.Context, store *state.Store, opts *options, o wipGCOptions) (wipGCResult, int, error) {
	rows, err := store.DeviceWipAll(ctx)
	if err != nil {
		return wipGCResult{}, 0, err
	}
	projects, err := store.ListProjects(ctx)
	if err != nil {
		return wipGCResult{}, 0, err
	}
	if o.ProjectPath != "" {
		p, err := store.ProjectByPath(ctx, o.ProjectPath)
		if err != nil {
			return wipGCResult{}, 0, err
		}
		projects = []state.ProjectStatus{p}
	}
	device, err := store.CurrentDevice(ctx)
	if err != nil {
		return wipGCResult{}, 0, err
	}
	devices, err := store.ListDevices(ctx)
	if err != nil {
		return wipGCResult{}, 0, err
	}
	trust := make(map[string]string, len(devices))
	for _, d := range devices {
		trust[d.ID] = d.TrustState
	}
	orphans := map[string]wipOrphanRecord{}
	if raw, ok, metaErr := store.GetLocalMeta(ctx, wipGCOrphansKey); metaErr != nil {
		return wipGCResult{}, 0, metaErr
	} else if ok && json.Unmarshal([]byte(raw), &orphans) != nil {
		orphans = map[string]wipOrphanRecord{}
	}
	r := gitRunner(opts)
	remote := map[string][]dsgit.RemoteRef{}
	projectByKey := map[string]state.ProjectStatus{}
	result := wipGCResult{TTL: o.TTL.String(), DryRun: o.DryRun}
	for _, p := range projects {
		if p.Type != "git_repo" || p.LocalPath == "" {
			continue
		}
		projectByKey[p.PathKey] = p
		refs, skipped, listErr := r.LsRemoteWipRefs(ctx, p.LocalPath, "origin")
		if listErr != nil {
			result.Warnings = append(result.Warnings, wipGCWarning{Path: p.Path, Message: listErr.Error()})
			continue
		}
		remote[p.PathKey] = refs
		if skipped > 0 {
			result.Warnings = append(result.Warnings, wipGCWarning{Path: p.Path, Message: fmt.Sprintf("skipped %d malformed remote ref line(s)", skipped)})
		}
	}
	filteredRows := rows[:0]
	for _, row := range rows {
		if _, ok := projectByKey[row.PathKey]; ok {
			filteredRows = append(filteredRows, row)
		}
	}
	actions, nextOrphans := planWipGC(filteredRows, remote, trust, device.ID, o.TargetDevice, orphans, o.TTL, time.Now())
	for i := range actions {
		if actions[i].Path == "" {
			if p, ok := projectByKey[actions[i].PathKey]; ok {
				actions[i].Path = p.Path
			} else {
				actions[i].Path = actions[i].PathKey
			}
		}
	}
	failedDeletes := 0
	for i := range actions {
		a := &actions[i]
		if !a.Delete || o.DryRun {
			continue
		}
		p, ok := projectByKey[a.PathKey]
		if !ok {
			a.Delete, a.Reason = false, "project unavailable; not deleted"
			continue
		}
		if err := r.FetchRef(ctx, p.LocalPath, "origin", a.Ref); err != nil {
			a.Delete, a.Reason = false, "could not fetch candidate; not deleted"
			result.Warnings = append(result.Warnings, wipGCWarning{Path: p.Path, Message: err.Error()})
			continue
		}
		old, err := commitAgeExceeds(ctx, r, p.LocalPath, a.SHA, o.TTL, time.Now())
		if err != nil {
			a.Delete, a.Reason = false, "could not inspect object; not deleted"
			result.Warnings = append(result.Warnings, wipGCWarning{Path: p.Path, Message: err.Error()})
			continue
		}
		if _, err := r.Run(ctx, p.LocalPath, "update-ref", "-d", a.Ref); err != nil {
			a.Delete, a.Reason = false, "could not remove local inspection ref; not deleted"
			result.Warnings = append(result.Warnings, wipGCWarning{Path: p.Path, Message: err.Error()})
			continue
		}
		if !corroborateCommitAge(a, old) {
			continue
		}
		if strings.HasSuffix(a.Reason, "device past TTL") {
			fresh, err := store.ListDevices(ctx)
			if err != nil {
				a.Delete, a.Reason = false, "could not re-verify device trust; not deleted"
				result.Warnings = append(result.Warnings, wipGCWarning{Path: p.Path, Message: err.Error()})
				continue
			}
			stillTerminal := false
			for _, d := range fresh {
				if d.ID == a.DeviceID {
					stillTerminal = d.TrustState == "revoked" || d.TrustState == "lost"
					break
				}
			}
			if !stillTerminal {
				a.Delete, a.Reason = false, "device trust changed during sweep; not deleted"
				continue
			}
		}
		if err := dropWipRef(ctx, store, r, p, a.DeviceID, a.SHA); err != nil {
			a.Delete, a.Reason = false, "delete failed"
			result.Warnings = append(result.Warnings, wipGCWarning{Path: p.Path, Message: err.Error()})
			failedDeletes++
		}
	}
	result.Actions = actions
	if !o.DryRun {
		raw, err := json.Marshal(nextOrphans)
		if err != nil {
			return wipGCResult{}, failedDeletes, err
		}
		if err := store.SetLocalMeta(ctx, wipGCOrphansKey, string(raw)); err != nil {
			return wipGCResult{}, failedDeletes, err
		}
	}
	return result, failedDeletes, nil
}

func newWipGCCommand(stdout io.Writer, opts *options) *cobra.Command {
	var targetDevice, ttlText string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "gc [<project>]",
		Short: "Expire aged WIP recovery refs",
		Args:  usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			ttl, err := time.ParseDuration(ttlText)
			if err != nil || ttl <= 0 {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("invalid --ttl %q", ttlText)}
			}
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			projectPath := ""
			if len(args) == 1 {
				projectPath = args[0]
			}
			result, failedDeletes, err := sweepWipRefs(cmd.Context(), store, opts, wipGCOptions{
				TargetDevice: targetDevice,
				ProjectPath:  projectPath,
				TTL:          ttl,
				DryRun:       dryRun,
			})
			if err != nil {
				return err
			}
			renderErr := opts.render(stdout, func(w io.Writer) error {
				for _, a := range result.Actions {
					verb := "keep"
					if a.Delete {
						verb = "delete"
					}
					if _, err := fmt.Fprintf(w, "%s %s %s: %s\n", verb, a.Path, a.Ref, a.Reason); err != nil {
						return err
					}
				}
				for _, warning := range result.Warnings {
					if _, err := fmt.Fprintf(w, "warning %s: %s\n", warning.Path, warning.Message); err != nil {
						return err
					}
				}
				return nil
			}, result)
			if renderErr != nil {
				return renderErr
			}
			// A delete that was NOMINATED and then FAILED is a different outcome
			// class from a clean sweep, and an unattended caller (the sync-cycle
			// sweep is the obvious next consumer) cannot tell them apart from a
			// zero exit alone. Enumeration failures stay warnings — not finding
			// a remote is not the same as failing to delete from one.
			if failedDeletes > 0 {
				return appError{code: exitGit, err: fmt.Errorf("%d WIP ref deletion(s) failed; see warnings", failedDeletes)}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&targetDevice, "device", "", "explicitly reap this device's aged refs")
	cmd.Flags().StringVar(&ttlText, "ttl", defaultWipGCTTL.String(), "minimum age before deletion")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "plan and enumerate without deleting or persisting orphan state")
	return cmd
}
