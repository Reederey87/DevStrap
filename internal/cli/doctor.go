package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/Reederey87/DevStrap/internal/config"
	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/hub"
	"github.com/Reederey87/DevStrap/internal/platform"
	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
	"github.com/spf13/cobra"
)

// hubMetricsCapable is the optional capability a hub backend implements to
// expose its accumulated op/byte counters for `doctor --remote` (P4-HUB-14).
// Declared at package scope so it can name hub.MetricsSnapshot without the
// local `hub` variable inside checkHubHealth shadowing the package.
type hubMetricsCapable interface {
	HubMetrics() (hub.MetricsSnapshot, bool)
}

// checkStatus is the severity grade for a doctor check (PROD-02).
type checkStatus string

const (
	checkOK    checkStatus = "ok"
	checkWarn  checkStatus = "warning"
	checkError checkStatus = "error"
)

// checkResult is one graded doctor check. Remedy is a human-readable
// remediation hint (or the action --fix performed).
type checkResult struct {
	Name   string      `json:"name"`
	Status checkStatus `json:"status"`
	Detail string      `json:"detail,omitempty"`
	Remedy string      `json:"remedy,omitempty"`
}

func newDoctorCommand(stdout io.Writer, opts *options) *cobra.Command {
	var fixFlag bool
	var remoteFlag bool
	var hubFile string
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check local prerequisites and workspace health",
		RunE: func(cmd *cobra.Command, args []string) error {
			results := runDoctorChecks(cmd.Context(), opts)
			if fixFlag {
				results = applyDoctorFixes(cmd.Context(), opts, results)
			}
			// P5-PROD-05: --remote probes the sync hub (reachability, pending
			// push backlog, queued hub deletes, device trust) so a fleet's
			// convergence health is visible, not just local prerequisites.
			if remoteFlag {
				results = append(results, checkHubHealth(cmd.Context(), opts, hubFile)...)
			}
			if err := opts.render(stdout, func(w io.Writer) error {
				renderDoctorResults(w, results)
				return nil
			}, results); err != nil {
				return err
			}
			errs := 0
			for _, r := range results {
				if r.Status == checkError {
					errs++
				}
			}
			if errs > 0 {
				return appError{code: exitGeneric, err: fmt.Errorf("doctor found %d error(s)", errs)}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&fixFlag, "fix", false, "apply safe remediations (create state home, run migrations, clear stale locks)")
	cmd.Flags().BoolVar(&remoteFlag, "remote", false, "also probe the sync hub (reachability, pending push, queued deletes, device trust)")
	cmd.Flags().StringVar(&hubFile, "hub-file", "", "file-backed test hub path for --remote")
	return cmd
}

// checkHubHealth probes the sync hub for --remote (P5-PROD-05): reachability,
// workspace-id visibility, pending-push backlog, queued hub-deletes, and a
// device-trust summary. For remote (workspace-id-keyed) hubs: r2/s3/git, it
// also probes for the joiner "never pulled / workspace id match" warning. It
// is a thin observability layer over the existing event log + cursors.
func checkHubHealth(ctx context.Context, opts *options, hubFile string) []checkResult {
	if _, err := os.Stat(opts.paths().StateDB()); err != nil {
		return []checkResult{{Name: "hub", Status: checkWarn, Detail: "no state database; run `devstrap init`"}}
	}
	store, err := opts.openState(ctx)
	if err != nil {
		return []checkResult{{Name: "hub", Status: checkError, Detail: err.Error()}}
	}
	defer closeStore(store)
	var out []checkResult
	if wsID, werr := store.WorkspaceID(ctx); werr == nil {
		out = append(out, checkResult{Name: "workspace id", Status: checkOK, Detail: wsID})
	} else {
		out = append(out, checkResult{Name: "workspace id", Status: checkWarn, Detail: werr.Error()})
	}
	hub, hubID, err := hubFromOptions(ctx, opts, store, hubFile)
	if err != nil {
		return append(out, checkResult{Name: "hub", Status: checkError, Detail: err.Error(), Remedy: "pass --hub-file or set 'hub' in config"})
	}
	if _, err := hub.ListBlobs(ctx); err != nil {
		return append(out, checkResult{Name: "hub reachable", Status: checkError, Detail: err.Error()})
	}
	out = append(out, checkResult{Name: "hub reachable", Status: checkOK, Detail: hubID})

	// P7-PROD-03: warn (never fail) on retention-manifest version skew — a
	// mixed-version fleet (e.g. a `brew upgrade` landing on some machines
	// before others) must be visible, not silently wedged. GetRetention
	// returning ErrRetentionNotFound just means no compaction has run yet,
	// which is not a skew signal. The version stamp is read WITHOUT going
	// through ParseRetentionManifest's fail-closed range check, so this warns
	// even for a manifest this binary's normal read path would refuse
	// outright — the whole point is to explain a wedge, not just hit it too.
	if retRaw, _, rerr := hub.GetRetention(ctx); rerr == nil {
		if v, minReader, verr := dssync.RetentionManifestVersionStamp(retRaw); verr == nil {
			cur := dssync.CurrentSnapshotVersion()
			switch {
			case minReader > cur:
				out = append(out, checkResult{
					Name:   "retention manifest version",
					Status: checkWarn,
					Detail: fmt.Sprintf("hub retention manifest declares min reader version %d; this binary reads up to %d", minReader, cur),
					Remedy: "upgrade devstrap on this device",
				})
			case v != cur:
				out = append(out, checkResult{
					Name:   "retention manifest version",
					Status: checkWarn,
					Detail: fmt.Sprintf("hub retention manifest is v%d; this binary produces v%d", v, cur),
					Remedy: "upgrade the device that last ran `devstrap hub compact` (or this device, if it is the one behind)",
				})
			}
		}
	} else if !errors.Is(rerr, dssync.ErrRetentionNotFound) {
		out = append(out, checkResult{Name: "retention manifest version", Status: checkWarn, Detail: rerr.Error()})
	}

	// P5-SYNC-01: the push watermark is the Seq-keyed row (with its one-time
	// legacy-HLC backfill), so doctor counts the same pending set sync pushes.
	pushCursor, _ := store.PushSeqCursor(ctx, hubID)
	if pending, perr := store.LocalPendingEventsBySeq(ctx, pushCursor); perr == nil {
		if len(pending) > 0 {
			out = append(out, checkResult{Name: "pending push", Status: checkWarn, Detail: fmt.Sprintf("%d local event(s) not yet pushed", len(pending)), Remedy: "run `devstrap sync`"})
		} else {
			out = append(out, checkResult{Name: "pending push", Status: checkOK, Detail: "0"})
		}
	}
	if isRemoteHubID(hubID) {
		// Probe the RAW backend for prefix emptiness — never through the
		// EncryptedHub (a diagnostic must not run grant ingestion). The
		// backend is unwrapped from the hub already built above so doctor
		// does not resolve hub credentials (keychain / `op read`) twice.
		var rawBackend dssync.Hub
		if enc, ok := hub.(dssync.EncryptedHub); ok {
			rawBackend = enc.Hub
		}
		if rawBackend != nil {
			type hasEventsCapable interface {
				HasEvents(ctx context.Context) (bool, error)
			}
			if hec, ok := rawBackend.(hasEventsCapable); ok {
				hasEvents, herr := hec.HasEvents(ctx)
				// A cursor read error degrades to "never pulled", biasing
				// toward the warning; acceptable because HasEvents==false (a
				// probe that SUCCEEDED against a genuinely empty prefix) must
				// also hold, matching the pushCursor convention above.
				// P5-SYNC-01: "has this device ever pulled" is now answered by
				// the per-device PULL cursor rows (push watermarks deliberately
				// excluded — HubDeviceCursors omits "push:" rows, so a device
				// that pushed but never pulled still warns), with the frozen
				// legacy HLC row honored for pre-migration stores.
				var pullCursor int64
				if cursors, cerr := store.HubDeviceCursors(ctx, hubID); cerr == nil && len(cursors) > 0 {
					pullCursor = 1
				} else if legacy, lerr := store.HubCursor(ctx, hubID); lerr == nil {
					pullCursor = legacy
				}
				role := opts.v.GetString("role")
				if herr == nil && shouldWarnWorkspaceIDMismatch(role, hubID, pullCursor, hasEvents) {
					out = append(out, checkResult{
						Name:   "workspace id match",
						Status: checkWarn,
						Detail: "this device joined an existing workspace but sees zero hub events under its own workspace id; it may not share the founder's workspace id",
						Remedy: "confirm the founder's workspace id via `devstrap doctor` on the founding device, then re-init this device with `devstrap init --join --workspace-id <founder workspace id>`",
					})
				}
			}
		}
	}
	if queued, qerr := store.PendingHubDeletes(ctx); qerr == nil && len(queued) > 0 {
		out = append(out, checkResult{Name: "queued hub deletes", Status: checkWarn, Detail: fmt.Sprintf("%d superseded blob(s) awaiting deletion", len(queued)), Remedy: "run `devstrap sync`"})
	}
	if devices, derr := store.ListDevices(ctx); derr == nil {
		approved, revoked, other := 0, 0, 0
		for _, d := range devices {
			switch d.TrustState {
			case "approved":
				approved++
			case "revoked", "lost":
				revoked++
			default:
				other++
			}
		}
		out = append(out, checkResult{Name: "device trust", Status: checkOK, Detail: fmt.Sprintf("%d approved, %d revoked/lost, %d pending", approved, revoked, other)})
	}
	// P4-HUB-14: report the op/byte counters this probe accumulated on the hub
	// backend. The counters are per-process, so this reflects the doctor probe's
	// own I/O — a small, honest observability surface that also makes the hub's
	// I/O instrumentation visible in the health report. The metered backend sits
	// under the EncryptedHub decorator, so unwrap it first.
	raw := hub
	if enc, ok := hub.(dssync.EncryptedHub); ok {
		raw = enc.Hub
	}
	if mc, ok := raw.(hubMetricsCapable); ok {
		if snap, has := mc.HubMetrics(); has {
			out = append(out, checkResult{Name: "hub metrics", Status: checkOK, Detail: snap.String()})
		}
	}
	return out
}

// shouldWarnWorkspaceIDMismatch reports whether doctor's --remote probe should
// warn that this device's locally-minted workspace id may not match the
// founder's for remote (workspace-id-keyed) hubs: r2/s3/git/folder. It is
// deliberately pure so the heuristic can be table-tested without state or hub
// I/O (P4-SEC-07 pairing wave).
func shouldWarnWorkspaceIDMismatch(role string, hubID string, pullCursor int64, hasEvents bool) bool {
	isJoinerRole := strings.EqualFold(strings.TrimSpace(role), "joiner")
	return isJoinerRole && isRemoteHubID(hubID) && pullCursor == 0 && !hasEvents
}

func isRemoteHubID(hubID string) bool {
	return strings.HasPrefix(hubID, "r2:") || strings.HasPrefix(hubID, "s3:") ||
		strings.HasPrefix(hubID, "git:") || strings.HasPrefix(hubID, "folder:")
}

// runDoctorChecks collects all health checks into a graded result list (PROD-02).
func runDoctorChecks(ctx context.Context, opts *options) []checkResult {
	paths := opts.paths()
	var results []checkResult
	var serviceStore *state.Store
	if info, ok := debug.ReadBuildInfo(); ok {
		results = append(results, checkResult{Name: "go runtime", Status: checkOK, Detail: info.GoVersion})
	}
	results = append(results, checkResult{Name: "devstrap home", Status: checkOK, Detail: paths.Home})
	results = append(results, checkResult{Name: "managed root", Status: checkOK, Detail: paths.Root})
	// Tools: git is required; gh and go are optional.
	results = append(results, checkTool("git", true))
	results = append(results, checkTool("gh", false))
	results = append(results, checkTool("go", false))
	results = append(results, checkStateHome(paths)...)
	results = append(results, checkRestoreJournal(paths.Home)...)
	if _, err := os.Stat(paths.StateDB()); err == nil {
		store, err := opts.openState(ctx)
		if err != nil {
			results = append(results, checkResult{Name: "state database", Status: checkError, Detail: err.Error()})
		} else {
			defer closeStore(store)
			serviceStore = store
			results = append(results, checkDB(ctx, store)...)
			results = append(results, checkDanglingBlobRefs(ctx, paths, store)...)
			results = append(results, checkSecretsRotation(ctx, store)...)
			results = append(results, checkDeviceKeys(ctx, paths, store)...)
			results = append(results, checkKeyGrantWaits(ctx, store)...)

			results = append(results, checkSkippedEvents(ctx, store)...)
			results = append(results, checkHubHashChainConflicts(ctx, store)...)
			results = append(results, checkDurabilityExport(ctx, opts, store, time.Now())...)
			results = append(results, checkWorkspaceKeyAge(ctx, opts, store)...)
			results = append(results, checkWCKRotationPending(ctx, store)...)
			results = append(results, checkRevokeContainmentPending(ctx, store)...)
			results = append(results, checkForgeCLIs(ctx, opts, store)...)
			results = append(results, checkAgentRunSweep(ctx, opts, store)...)
			results = append(results, checkSandboxViolations(ctx, store)...)
			results = append(results, checkFailedMaterializations(ctx, store)...)
			results = append(results, checkBloblessCaveat(ctx, store)...)
		}
	} else if os.IsNotExist(err) {
		results = append(results, checkResult{Name: "state database", Status: checkWarn, Detail: "missing", Remedy: "run `devstrap init` (or doctor --fix)"})
	} else {
		results = append(results, checkResult{Name: "state database", Status: checkError, Detail: err.Error()})
	}
	results = append(results, checkRepoLocks(paths.Home)...)
	results = append(results, checkService(ctx, opts, serviceStore)...)
	return results
}

// checkHubHashChainConflicts elevates unexplained origin-stream breaks. A
// missing workspace-key grant can produce the same conflict transiently: an
// undecryptable predecessor is quarantined, then its successor cannot join the
// local chain until the grant arrives and replay restores the predecessor. The
// open grant wait and preserved carrier let doctor distinguish that documented,
// self-healing P6-SEC-03 hold from a break with no such explanation.
func checkHubHashChainConflicts(ctx context.Context, store *state.Store) []checkResult {
	conflicts, err := store.OpenConflictsByType(ctx, dssync.ConflictEventHashChain)
	if err != nil {
		return []checkResult{{Name: "hub hash-chain integrity", Status: checkError, Detail: err.Error()}}
	}
	if len(conflicts) == 0 {
		return []checkResult{{Name: "hub hash-chain integrity", Status: checkOK, Detail: "0 open hash-chain breaks"}}
	}
	pendingPredecessors, err := devicesAwaitingPredecessorGrant(ctx, store)
	if err != nil {
		return []checkResult{{Name: "hub hash-chain integrity", Status: checkError, Detail: err.Error()}}
	}
	explained := 0
	for _, conflict := range conflicts {
		var details struct {
			DeviceID string `json:"device_id"`
			Seq      int64  `json:"seq"`
		}
		if json.Unmarshal([]byte(conflict.DetailsJSON), &details) == nil {
			for _, predecessorSeq := range pendingPredecessors[details.DeviceID] {
				if predecessorSeq > 0 && predecessorSeq < details.Seq {
					explained++
					break
				}
			}
		}
	}
	unexplained := len(conflicts) - explained
	if unexplained == 0 {
		return []checkResult{{
			Name:   "hub hash-chain integrity",
			Status: checkWarn,
			Detail: fmt.Sprintf("%d hash-chain hold(s) explained by pending workspace key grant(s); expected to auto-resolve after grant replay", explained),
			Remedy: "follow the awaiting-key-grants remedy; quarantined predecessors replay automatically after the grant arrives",
		}}
	}
	detail := fmt.Sprintf("%d unexplained open %s conflict(s): possible hub data loss, truncation, or corruption", unexplained, dssync.ConflictEventHashChain)
	if explained > 0 {
		detail += fmt.Sprintf(" (%d additional hold(s) explained by pending key grants)", explained)
	}
	return []checkResult{{
		Name:   "hub hash-chain integrity",
		Status: checkError,
		Detail: detail,
		Remedy: "inspect with `devstrap conflicts list`; stop destructive hub maintenance and recover from a verified durability replica if the primary lost data",
	}}
}

// devicesAwaitingPredecessorGrant returns origin devices whose preserved
// undecryptable carrier names an epoch/kid that is still in key_grant_waits.
// key_grant_waits is workspace-scoped rather than device-scoped, so the
// carrier is the necessary bridge from a pending epoch to the hash conflict's
// origin device.
func devicesAwaitingPredecessorGrant(ctx context.Context, store *state.Store) (map[string][]int64, error) {
	waits, err := store.OpenKeyGrantWaits(ctx)
	if err != nil {
		return nil, err
	}
	if len(waits) == 0 {
		return map[string][]int64{}, nil
	}
	waiting := func(epoch int64, kid string) bool {
		for _, wait := range waits {
			if wait.Epoch == epoch && (wait.KID == "" || kid == "" || wait.KID == kid) {
				return true
			}
		}
		return false
	}
	verificationConflicts, err := store.OpenConflictsByType(ctx, dssync.ConflictEventVerification)
	if err != nil {
		return nil, err
	}
	devices := make(map[string][]int64)
	for _, conflict := range verificationConflicts {
		var details struct {
			Kind      string `json:"kind"`
			DeviceID  string `json:"device_id"`
			EventJSON string `json:"event_json"`
		}
		if json.Unmarshal([]byte(conflict.DetailsJSON), &details) != nil ||
			details.Kind != dssync.EventConflictKindUndecryptable || details.DeviceID == "" {
			continue
		}
		var carrier state.Event
		if json.Unmarshal([]byte(details.EventJSON), &carrier) != nil || carrier.DeviceID != details.DeviceID {
			continue
		}
		envelope, err := dssync.ParseEncryptedEnvelope(carrier)
		if err == nil && waiting(envelope.Epoch, envelope.KID) {
			devices[details.DeviceID] = append(devices[details.DeviceID], carrier.Seq)
		}
	}
	return devices, nil
}

// checkDurabilityExport reports only actionable staleness for an opted-in
// replica. An unconfigured replica is an OK informational row because hub
// replication is defense in depth, not a prerequisite for correct sync.
func checkDurabilityExport(ctx context.Context, opts *options, store *state.Store, now time.Time) []checkResult {
	replica := strings.TrimSpace(opts.v.GetString("hub_replica"))
	if replica == "" {
		return []checkResult{{Name: "hub durability export", Status: checkOK, Detail: "not configured (optional; set hub_replica for namespace-metadata bucket-loss protection)"}}
	}
	interval, err := durabilityExportInterval(opts)
	if err != nil {
		return []checkResult{{Name: "hub durability export", Status: checkWarn, Detail: err.Error(), Remedy: "set durability.export_interval to a duration such as 24h, or 0 to disable"}}
	}
	if interval == 0 {
		return []checkResult{{Name: "hub durability export", Status: checkOK, Detail: "disabled by durability.export_interval=0"}}
	}
	record, ok, err := readDurabilityExportRecord(ctx, store)
	if err != nil {
		return []checkResult{{Name: "hub durability export", Status: checkWarn, Detail: err.Error(), Remedy: "run `devstrap sync` or `devstrap run-loop --once` to replace the invalid record"}}
	}
	if !ok || record.Replica != replica {
		return []checkResult{{Name: "hub durability export", Status: checkWarn, Detail: "no successful export recorded for the configured replica", Remedy: "run `devstrap hub compact`, then `devstrap sync` or `devstrap run-loop --once`"}}
	}
	age := now.Sub(record.ExportedAt)
	if age < 0 {
		return []checkResult{{Name: "hub durability export", Status: checkWarn, Detail: fmt.Sprintf("last success timestamp is %s in the future", record.ExportedAt.UTC().Format(time.RFC3339)), Remedy: "correct the system clock, then re-run `devstrap sync`"}}
	}
	staleAfter := interval * 2
	if interval > time.Duration(math.MaxInt64/2) {
		staleAfter = time.Duration(math.MaxInt64)
	}
	detail := fmt.Sprintf("last success %s ago (snapshot %s; interval %s)", age.Round(time.Second), record.SnapshotSHA256, interval)
	if age > staleAfter {
		return []checkResult{{Name: "hub durability export", Status: checkWarn, Detail: detail, Remedy: "re-run `devstrap sync` or `devstrap run-loop --once`; verify replica credentials and reachability if it fails"}}
	}
	return []checkResult{{Name: "hub durability export", Status: checkOK, Detail: detail}}
}

func checkRestoreJournal(home string) []checkResult {
	journalPath := restoreJournalPath(home)
	if _, err := os.Stat(journalPath); err == nil {
		return []checkResult{{Name: "restore journal", Status: checkError, Detail: "interrupted restore detected at " + journalPath, Remedy: "run `devstrap db restore --recover`"}}
	} else if !errors.Is(err, os.ErrNotExist) {
		return []checkResult{{Name: "restore journal", Status: checkError, Detail: err.Error(), Remedy: "run `devstrap db restore --recover`"}}
	}
	return nil
}

// checkService reports the background run-loop service's health (P4-PROD-04).
// It is entirely optional: an unsupported platform/session omits the check, a
// not-installed service reports ok (with the install hint), a running service
// reports ok, and an installed-but-stopped service warns with an inspection
// remedy.
func checkService(ctx context.Context, opts *options, store *state.Store) []checkResult {
	mgr := serviceBackend()
	label := mgr.DefaultLabel()
	status, err := mgr.Status(ctx, label)
	if err != nil {
		if errors.Is(err, platform.ErrUnsupported) {
			return nil
		}
		return []checkResult{{Name: "run-loop service", Status: checkWarn, Detail: err.Error()}}
	}
	if !status.Installed {
		return []checkResult{{Name: "run-loop service", Status: checkOK, Detail: "not installed (optional; `devstrap service install` for unattended sync)"}}
	}
	var custodyResult []checkResult
	if store != nil {
		recorded, custodyErr := store.KeyCustody(ctx)
		if custodyErr != nil {
			custodyResult = append(custodyResult, checkResult{Name: "run-loop service custody", Status: checkWarn, Detail: custodyErr.Error()})
		} else if state.EffectiveKeyCustody(recorded) == devicekeys.CustodyKeychain {
			detail := "keychain-custody store under an unattended service; a locked keychain makes ticks fail closed"
			remedy := fmt.Sprintf("re-initialize with %s=1 and migrate the key files to file custody", platform.NoKeychainEnv)
			if mgr.Name() == "systemd-user" {
				detail = "keychain-custody store under an unattended service; the systemd user unit has no session D-Bus and fails closed every tick"
				remedy += ", or reinstall with --allow-keychain-custody if a user-session D-Bus is available at service runtime"
			}
			custodyResult = append(custodyResult, checkResult{Name: "run-loop service custody", Status: checkWarn, Detail: detail, Remedy: remedy})
		}
	}
	if status.ExecPathMissing {
		return append([]checkResult{{
			Name:   "run-loop service",
			Status: checkWarn,
			Detail: fmt.Sprintf("installed service ExecPath is missing: %s", status.ExecPath),
			Remedy: "re-run devstrap service install (the installed unit points at a binary that no longer exists — e.g. after a brew upgrade)",
		}}, custodyResult...)
	}
	if status.Running {
		return append([]checkResult{{Name: "run-loop service", Status: checkOK, Detail: fmt.Sprintf("installed and running (%s)", status.Detail)}}, custodyResult...)
	}
	return append([]checkResult{{
		Name:   "run-loop service",
		Status: checkWarn,
		Detail: fmt.Sprintf("installed but not running (%s)", status.Detail),
		Remedy: fmt.Sprintf("inspect launchctl print / journalctl --user -u %s; reinstall with devstrap service install", label),
	}}, custodyResult...)
}

func checkAgentRunSweep(ctx context.Context, opts *options, store *state.Store) []checkResult {
	_ = opts
	reconciled, stillRunning, err := sweepStaleAgentRuns(ctx, store)
	if err != nil {
		return []checkResult{{Name: "agent run sweep", Status: checkWarn, Detail: err.Error()}}
	}
	status := checkOK
	if reconciled > 0 {
		status = checkWarn
	}
	result := checkResult{
		Name:   "agent run sweep",
		Status: status,
		Detail: fmt.Sprintf("%d reconciled to interrupted; %d still running", reconciled, stillRunning),
	}
	if reconciled > 0 {
		result.Remedy = "review interrupted runs with `devstrap agent show`; rerun the agent before PR unless using --allow-incomplete"
	}
	return []checkResult{result}
}

func checkSandboxViolations(ctx context.Context, store *state.Store) []checkResult {
	n, err := store.CountRunsWithSandboxViolations(ctx)
	if err != nil {
		return nil
	}
	if n == 0 {
		return []checkResult{{Name: "agent sandbox violations", Status: checkOK, Detail: "0"}}
	}
	return []checkResult{{
		Name:   "agent sandbox violations",
		Status: checkWarn,
		Detail: fmt.Sprintf("%d run(s) with denials", n),
		Remedy: "inspect with `devstrap agent show <run-id>`; a denial means the agent tried an operation the OS sandbox blocked — expected for hostile/buggy tools, investigate unexpected ones",
	}}
}

// checkFailedMaterializations surfaces projects whose last materialize attempt
// left materialization_state=failed (P4-GIT-07). Detail carries the scrubbed
// last_error text when present so operators can see why without grepping logs.
func checkFailedMaterializations(ctx context.Context, store *state.Store) []checkResult {
	projects, err := store.ListProjects(ctx)
	if err != nil {
		return []checkResult{{Name: "failed materializations", Status: checkWarn, Detail: err.Error()}}
	}
	var out []checkResult
	for _, p := range projects {
		if p.MaterializationState != "failed" {
			continue
		}
		detail := p.LastError
		if detail == "" {
			detail = "materialization failed (no recorded error)"
		}
		out = append(out, checkResult{
			Name:   "materialize: " + p.Path,
			Status: checkWarn,
			Detail: detail,
			Remedy: "re-run 'devstrap materialize' or 'devstrap sync' to retry",
		})
	}
	if len(out) == 0 {
		return []checkResult{{Name: "failed materializations", Status: checkOK, Detail: "0"}}
	}
	return out
}

func checkTool(name string, required bool) checkResult {
	path, err := exec.LookPath(name)
	if err != nil {
		if required {
			return checkResult{Name: name, Status: checkError, Detail: "not found", Remedy: fmt.Sprintf("install %s", name)}
		}
		return checkResult{Name: name, Status: checkWarn, Detail: "not found", Remedy: fmt.Sprintf("optional; some features will be unavailable without %s", name)}
	}
	return checkResult{Name: name, Status: checkOK, Detail: path}
}

func checkStateHome(paths config.Paths) []checkResult {
	var out []checkResult
	stat, err := os.Stat(paths.Home)
	if err != nil {
		if os.IsNotExist(err) {
			out = append(out, checkResult{Name: "state home", Status: checkWarn, Detail: "missing", Remedy: "run `devstrap init` (or doctor --fix)"})
			return out
		}
		out = append(out, checkResult{Name: "state home", Status: checkError, Detail: err.Error()})
		return out
	}
	out = append(out, checkResult{Name: "state home", Status: checkOK, Detail: fmt.Sprintf("%s (mode %s)", paths.Home, stat.Mode().Perm())})
	return out
}

func checkDB(ctx context.Context, store *state.Store) []checkResult {
	var out []checkResult
	version, err := store.Version()
	if err != nil {
		return []checkResult{{Name: "schema", Status: checkError, Detail: err.Error()}}
	}
	out = append(out, checkResult{Name: "schema", Status: checkOK, Detail: fmt.Sprintf("version %d", version)})
	check, err := store.QuickCheck(ctx)
	if err != nil {
		out = append(out, checkResult{Name: "sqlite quick_check", Status: checkError, Detail: err.Error(), Remedy: "restore from a `devstrap db backup --full` archive (only a full backup recovers the encrypted secrets alongside the database)"})
	} else if check != "ok" {
		out = append(out, checkResult{Name: "sqlite quick_check", Status: checkError, Detail: check, Remedy: "restore from a `devstrap db backup --full` archive (only a full backup recovers the encrypted secrets alongside the database)"})
	} else {
		out = append(out, checkResult{Name: "sqlite quick_check", Status: checkOK, Detail: "ok"})
	}
	fkCheck, err := store.ForeignKeyCheck(ctx)
	if err != nil {
		out = append(out, checkResult{Name: "foreign_key_check", Status: checkError, Detail: err.Error()})
	} else if fkCheck != "ok" {
		out = append(out, checkResult{Name: "foreign_key_check", Status: checkError, Detail: fkCheck})
	} else {
		out = append(out, checkResult{Name: "foreign_key_check", Status: checkOK, Detail: "ok"})
	}
	return out
}

// checkDanglingBlobRefs verifies that every age blob the DB references has its
// ciphertext present on disk under blobs/ (P6-DATA-04). A referenced blob whose
// file is missing means the captured secret is unrecoverable — `env hydrate`
// for that profile will fail — so it is graded an error. This is exactly the
// wreckage left by restoring a DB-only backup: the refs survive, the secrets do
// not. The remedy points at a full-backup restore, which alone carries blobs.
func checkDanglingBlobRefs(ctx context.Context, paths config.Paths, store *state.Store) []checkResult {
	refs, err := store.AllBlobRefs(ctx)
	if err != nil {
		return nil
	}
	if len(refs) == 0 {
		return []checkResult{{Name: "blob refs", Status: checkOK, Detail: "0 referenced"}}
	}
	blobDir := filepath.Join(paths.Home, "blobs")
	var missing []string
	for _, ref := range refs {
		hash, herr := envBlobHash(ref)
		if herr != nil {
			missing = append(missing, ref)
			continue
		}
		if _, serr := os.Stat(filepath.Join(blobDir, hash+".age")); serr != nil {
			missing = append(missing, ref)
		}
	}
	if len(missing) == 0 {
		return []checkResult{{Name: "blob refs", Status: checkOK, Detail: fmt.Sprintf("%d referenced, all present", len(refs))}}
	}
	sort.Strings(missing)
	sample := missing
	if len(sample) > 3 {
		sample = sample[:3]
	}
	return []checkResult{{
		Name:   "dangling blob refs",
		Status: checkError,
		Detail: fmt.Sprintf("%d of %d referenced blob(s) missing on disk (e.g. %s)", len(missing), len(refs), strings.Join(sample, ", ")),
		Remedy: "restore from a `devstrap db backup --full` archive; a DB-only backup cannot recover these encrypted secrets",
	}}
}

func checkSecretsRotation(ctx context.Context, store *state.Store) []checkResult {
	rotate, err := store.CountSecretBindingsNeedingRotation(ctx)
	if err != nil {
		return nil
	}
	if rotate > 0 {
		return []checkResult{{Name: "secrets needing rotation", Status: checkWarn, Detail: fmt.Sprintf("%d (rotate at source after a device revoke)", rotate), Remedy: "rotate at the provider, then 'devstrap env rotate <path> <env-file>' to re-capture and clear the flag (or 'devstrap env rotate --all')"}}
	}
	return []checkResult{{Name: "secrets needing rotation", Status: checkOK, Detail: "0"}}
}

// checkForgeCLIs iterates adopted git-repo remotes, resolves the forge for
// each (with --forge/project/host-map overrides), and warns when the matching
// forge CLI (gh/glab/tea) is missing or the forge is unknown — so the failure
// surfaces in doctor rather than only at `agent pr` time (GIT-05).
func checkForgeCLIs(ctx context.Context, opts *options, store *state.Store) []checkResult {
	projects, err := store.ListProjects(ctx)
	if err != nil {
		return nil
	}
	hostMap := forgeHostMap(opts.v)
	var out []checkResult
	seen := make(map[string]bool)
	for _, p := range projects {
		if p.Type != "git_repo" || p.RemoteURL == "" {
			continue
		}
		kind := ResolveForge(ctx, p.RemoteURL, "", p.ForgeKind, hostMap)
		cli := forgeCLI(kind)
		if cli == "" {
			if !seen["unknown"] {
				out = append(out, checkResult{Name: "forge cli", Status: checkWarn, Detail: fmt.Sprintf("unknown forge for %s", forgeHost(p.RemoteURL)), Remedy: "set a [forge] host map, git_repos.forge_kind, or pass --forge (GIT-05)"})
				seen["unknown"] = true
			}
			continue
		}
		if _, err := exec.LookPath(cli); err != nil {
			out = append(out, checkResult{Name: "forge cli " + cli, Status: checkWarn, Detail: fmt.Sprintf("missing (%s for %s)", cli, forgeHost(p.RemoteURL)), Remedy: fmt.Sprintf("install %s for %s PR creation", cli, kind)})
		}
	}
	return out
}

// checkBloblessCaveat surfaces the offline caveat for blobless clones (GIT-06):
// `git blame`/`log -p` on a partial clone trigger per-object lazy fetches that
// need the promisor remote online. It is informational (ok) so it is visible in
// the graded report without inflating the warning count.
func checkBloblessCaveat(ctx context.Context, store *state.Store) []checkResult {
	projects, err := store.ListProjects(ctx)
	if err != nil {
		return nil
	}
	gitRepos := 0
	for _, p := range projects {
		if p.Type == "git_repo" {
			gitRepos++
		}
	}
	if gitRepos == 0 {
		return nil
	}
	return []checkResult{{Name: "blobless clone caveat", Status: checkOK, Detail: fmt.Sprintf("%d git repo(s) may use blobless clones; historical blobs need the remote online (lazy fetch)", gitRepos), Remedy: "enable materialization.maintenance for a post-clone prefetch; stay online for first blame/log -p"}}
}

// checkKeyGrantWaits surfaces workspace keys this device has seen ciphertext
// for but never been granted (P6-SEC-03). An open wait means pulled events at
// those epochs are deferring (within the grace window) or quarantining (past
// it) — either way this device cannot read part of the fleet's event log until
// an approved device re-grants its held epochs.
// checkWorkspaceKeyAge grades the active WCK epoch's age against
// keys.rotate_max_age (P4-SEC-07 periodic rotation): ok at epoch 0 (the key is
// founded on the first sync), ok with the age while within the deadline, warn
// past it with the rotate remedy. The grading itself is pure
// (gradeWorkspaceKeyAge) so it can be table-tested without a store.
func checkWorkspaceKeyAge(ctx context.Context, opts *options, store *state.Store) []checkResult {
	epoch, created, err := store.ActiveKeyEpochAge(ctx)
	if err != nil {
		return nil
	}
	return []checkResult{gradeWorkspaceKeyAge(epoch, created, keyRotateMaxAge(opts), time.Now())}
}

func gradeWorkspaceKeyAge(epoch int64, created time.Time, maxAge time.Duration, now time.Time) checkResult {
	if epoch == 0 {
		return checkResult{Name: "workspace key age", Status: checkOK, Detail: "no workspace key yet (founded on first sync)"}
	}
	age := now.Sub(created).Truncate(time.Minute)
	detail := fmt.Sprintf("epoch %d, age %s", epoch, age)
	if maxAge > 0 && age > maxAge {
		return checkResult{
			Name:   "workspace key age",
			Status: checkWarn,
			Detail: fmt.Sprintf("%s exceeds keys.rotate_max_age %s", detail, maxAge),
			Remedy: "run 'devstrap keys rotate' (or 'devstrap sync', which rotates automatically), then sync so the grants publish",
		}
	}
	return checkResult{Name: "workspace key age", Status: checkOK, Detail: detail}
}

// checkWCKRotationPending surfaces an owed WCK rotation (issue #134): a device
// revoke could not rotate the epoch, so events keep sealing under a key the
// revoked device still holds until a rotation lands. Silent when nothing is
// owed — the marker exists only after a failed revoke-path rotation and is
// cleared only by this device's own successful Rotate (see wck_rotation.go).
func checkWCKRotationPending(ctx context.Context, store *state.Store) []checkResult {
	since, pending, err := wckRotationPendingSince(ctx, store)
	if err != nil || !pending {
		return nil
	}
	detail := "a device revoke could not rotate the workspace key; events remain readable by the revoked device"
	if !since.IsZero() {
		detail = fmt.Sprintf("owed since %s: %s", since.Format(time.RFC3339), detail)
	}
	return []checkResult{{
		Name:   "workspace key rotation",
		Status: checkWarn,
		Detail: detail,
		Remedy: "run 'devstrap sync' (retries the rotation automatically) or 'devstrap keys rotate'",
	}}
}

func checkRevokeContainmentPending(ctx context.Context, store *state.Store) []checkResult {
	devices, pending, malformed, err := revokeContainmentPending(ctx, store)
	if err != nil || !pending {
		return nil
	}
	detail := "pending record is malformed (kept pending fail-closed)"
	if !malformed {
		parts := make([]string, 0, len(devices))
		for _, id := range sortedPendingRevokeIDs(devices) {
			parts = append(parts, fmt.Sprintf("%s since %s", id, devices[id].Format(time.RFC3339)))
		}
		detail = strings.Join(parts, ", ")
	}
	return []checkResult{{
		Name:   "revoke containment",
		Status: checkWarn,
		Detail: detail,
		Remedy: "run devstrap sync to resume containment",
	}}
}

// checkSkippedEvents surfaces the durable P6-SYNC-02 skip records: hub
// objects this device's pulls keep dropping, each holding its origin device's
// cursor at a seq gap until it applies. The remedy depends on the reason.
func checkSkippedEvents(ctx context.Context, store *state.Store) []checkResult {
	skipped, err := store.OpenSkippedEvents(ctx)
	if err != nil {
		return nil
	}
	if len(skipped) == 0 {
		return []checkResult{{Name: "skipped hub events", Status: checkOK, Detail: "0"}}
	}
	byReason := map[string]int{}
	for _, rec := range skipped {
		byReason[rec.Reason]++
	}
	parts := make([]string, 0, len(byReason))
	for reason, n := range byReason {
		parts = append(parts, fmt.Sprintf("%s: %d", reason, n))
	}
	sort.Strings(parts)
	return []checkResult{{
		Name:   "skipped hub events",
		Status: checkWarn,
		Detail: strings.Join(parts, ", "),
		Remedy: "unknown-envelope-version: upgrade devstrap on this device; retired-enc-v1: re-found the workspace on a fresh hub; plaintext-anti-downgrade: the hub is serving plaintext where ciphertext is required — investigate the hub",
	}}
}

func checkKeyGrantWaits(ctx context.Context, store *state.Store) []checkResult {
	waits, err := store.OpenKeyGrantWaits(ctx)
	if err != nil {
		return nil
	}
	if len(waits) == 0 {
		return []checkResult{{Name: "awaiting key grants", Status: checkOK, Detail: "0"}}
	}
	labels := make([]string, 0, len(waits))
	for _, w := range waits {
		label := fmt.Sprintf("epoch %d", w.Epoch)
		if w.KID != "" {
			label += fmt.Sprintf(" (kid %.8s…)", w.KID)
		}
		label += fmt.Sprintf(" since %s", w.FirstSeen.UTC().Format("2006-01-02T15:04:05Z"))
		labels = append(labels, label)
	}
	return []checkResult{{
		Name:   "awaiting key grants",
		Status: checkWarn,
		Detail: strings.Join(labels, "; "),
		Remedy: "on a device that holds these epochs, run 'devstrap devices approve <this device id>' to re-grant every held epoch, then 'devstrap sync' on both devices (quarantined events replay automatically)",
	}}
}

func checkDeviceKeys(ctx context.Context, paths config.Paths, store *state.Store) []checkResult {
	device, err := store.CurrentDevice(ctx)
	if err != nil {
		return []checkResult{{Name: "device key", Status: checkWarn, Detail: "no current device"}}
	}
	recorded, err := store.KeyCustody(ctx)
	if err != nil {
		return []checkResult{{Name: "key custody", Status: checkError, Detail: err.Error()}}
	}
	keyStore := devicekeys.NewHybridStore(paths.KeyDir(), keychainBackend()).
		WithCustody(state.EffectiveKeyCustody(recorded))
	var out []checkResult
	out = append(out, keyCustodyStatus(ctx, keyStore, recorded))
	out = append(out, gradedKeyStatus("device key", deviceKeyStatus(ctx, keyStore, device.ID, device.PublicKey)))
	out = append(out, gradedKeyStatus("device signing key", deviceSigningKeyStatus(ctx, keyStore, device.ID, device.SigningPublicKey)))
	return out
}

// keyCustodyStatus reports the recorded key-custody backend and warns when the
// recorded backend is currently unreachable or is being overridden this run
// (P6-XP-04).
func keyCustodyStatus(ctx context.Context, keyStore devicekeys.HybridStore, recorded devicekeys.Custody) checkResult {
	if recorded == devicekeys.CustodyUnset {
		return checkResult{
			Name:   "key custody",
			Status: checkWarn,
			Detail: "not recorded (pre-P6-XP-04 store)",
			Remedy: "run `devstrap init` to record the custody backend",
		}
	}
	if state.EffectiveKeyCustody(recorded) != recorded {
		return checkResult{
			Name:   "key custody",
			Status: checkWarn,
			Detail: fmt.Sprintf("recorded %s; %s is forcing file custody this run", recorded, platform.NoKeychainEnv),
		}
	}
	if recorded == devicekeys.CustodyKeychain && keyStore.Probe(ctx) != devicekeys.CustodyKeychain {
		return checkResult{
			Name:   "key custody",
			Status: checkWarn,
			Detail: "keychain (currently unreachable)",
			Remedy: fmt.Sprintf("run from your desktop session, or set %s=1 and migrate the key files", platform.NoKeychainEnv),
		}
	}
	return checkResult{Name: "key custody", Status: checkOK, Detail: string(recorded)}
}

func gradedKeyStatus(name, status string) checkResult {
	switch status {
	case "ok":
		return checkResult{Name: name, Status: checkOK, Detail: "ok"}
	default:
		return checkResult{Name: name, Status: checkError, Detail: status, Remedy: "re-run `devstrap init` to provision device keys"}
	}
}

// checkRepoLocks grades held repo locks; stale locks are warnings (PROD-02).
func checkRepoLocks(home string) []checkResult {
	entries, err := os.ReadDir(repoLockDir(home))
	if err != nil {
		if os.IsNotExist(err) {
			return []checkResult{{Name: "repo locks", Status: checkOK, Detail: "none held"}}
		}
		return []checkResult{{Name: "repo locks", Status: checkError, Detail: err.Error()}}
	}
	held := 0
	var out []checkResult
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".lock") {
			continue
		}
		projectID := strings.TrimSuffix(name, ".lock")
		info, exists, stale, err := readRepoLock(home, projectID)
		if err != nil {
			out = append(out, checkResult{Name: "repo lock " + projectID, Status: checkError, Detail: err.Error()})
			continue
		}
		if !exists {
			continue
		}
		held++
		if stale {
			out = append(out, checkResult{Name: "repo lock " + projectID, Status: checkWarn, Detail: fmt.Sprintf("stale (pid %d on %s, acquired %s)", info.PID, info.Hostname, info.AcquiredAt), Remedy: "run `devstrap doctor --fix` or `devstrap worktree unlock <path>`"})
		} else {
			out = append(out, checkResult{Name: "repo lock " + projectID, Status: checkOK, Detail: fmt.Sprintf("live (pid %d on %s)", info.PID, info.Hostname)})
		}
	}
	if held == 0 {
		out = append(out, checkResult{Name: "repo locks", Status: checkOK, Detail: "none held"})
	}
	return out
}

// applyDoctorFixes performs safe remediations and re-runs the checks so the
// returned results reflect the post-fix state (PROD-02).
func applyDoctorFixes(ctx context.Context, opts *options, results []checkResult) []checkResult {
	paths := opts.paths()
	fixed := []string{}
	// Missing state home.
	if _, err := os.Stat(paths.Home); os.IsNotExist(err) {
		if err := os.MkdirAll(paths.Home, 0o700); err == nil {
			fixed = append(fixed, "created state home")
		}
	}
	// Pending migrations + stale locks need an existing state DB.
	if _, err := os.Stat(paths.StateDB()); err == nil {
		if store, err := opts.openState(ctx); err == nil {
			if err := store.Migrate(); err == nil {
				fixed = append(fixed, "applied pending migrations")
			}
			closeStore(store)
		}
	}
	// Clear stale repo locks.
	entries, _ := os.ReadDir(repoLockDir(paths.Home))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".lock") {
			continue
		}
		projectID := strings.TrimSuffix(name, ".lock")
		if _, exists, stale, err := readRepoLock(paths.Home, projectID); err == nil && exists && stale {
			if ok, lerr := clearRepoLock(paths.Home, projectID, true); ok {
				fixed = append(fixed, "cleared stale lock "+projectID)
			} else if lerr != nil {
				fixed = append(fixed, fmt.Sprintf("failed to clear stale lock %s: %v", projectID, lerr))
			}
		}
	}
	if len(fixed) > 0 {
		// Re-run checks to reflect the post-fix state.
		return runDoctorChecks(ctx, opts)
	}
	return results
}

// renderDoctorResults prints a graded table and a summary line.
func renderDoctorResults(stdout io.Writer, results []checkResult) {
	ok, warn, errs := 0, 0, 0
	for _, r := range results {
		switch r.Status {
		case checkOK:
			ok++
		case checkWarn:
			warn++
		case checkError:
			errs++
		}
		line := fmt.Sprintf("%-7s %-24s %s", r.Status, r.Name, r.Detail)
		if r.Remedy != "" {
			line += " — " + r.Remedy
		}
		_, _ = fmt.Fprintln(stdout, line)
	}
	_, _ = fmt.Fprintf(stdout, "\n%d ok, %d warning(s), %d error(s)\n", ok, warn, errs)
}

func deviceSigningKeyStatus(ctx context.Context, keyStore devicekeys.HybridStore, deviceID, publicKey string) string {
	if publicKey == "" {
		return "missing public key"
	}
	identity, err := keyStore.ReadSigning(ctx, deviceID)
	if err != nil {
		return "missing private identity"
	}
	if identity.Public != publicKey {
		return "public/private mismatch"
	}
	return "ok"
}

func deviceKeyStatus(ctx context.Context, keyStore devicekeys.HybridStore, deviceID, publicKey string) string {
	if publicKey == "" {
		return "missing public key"
	}
	identity, err := keyStore.Read(ctx, deviceID)
	if err != nil {
		return "missing private identity"
	}
	if identity.Recipient != publicKey {
		return "public/private mismatch"
	}
	return "ok"
}
