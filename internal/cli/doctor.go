package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime/debug"
	"strings"

	"github.com/Reederey87/DevStrap/internal/config"
	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/platform"
	"github.com/Reederey87/DevStrap/internal/state"
	"github.com/spf13/cobra"
)

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
			if opts.v.GetBool("json") {
				enc := json.NewEncoder(stdout)
				enc.SetIndent("", "  ")
				if err := enc.Encode(results); err != nil {
					return err
				}
			} else {
				renderDoctorResults(stdout, results)
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
// pending-push backlog, queued hub-deletes, and a device-trust summary. It is a
// thin observability layer over the existing event log + cursors.
func checkHubHealth(ctx context.Context, opts *options, hubFile string) []checkResult {
	if _, err := os.Stat(opts.paths().StateDB()); err != nil {
		return []checkResult{{Name: "hub", Status: checkWarn, Detail: "no state database; run `devstrap init`"}}
	}
	store, err := opts.openState(ctx)
	if err != nil {
		return []checkResult{{Name: "hub", Status: checkError, Detail: err.Error()}}
	}
	defer closeStore(store)
	hub, hubID, err := hubFromOptions(opts, hubFile)
	if err != nil {
		return []checkResult{{Name: "hub", Status: checkError, Detail: err.Error(), Remedy: "pass --hub-file or set 'hub' in config"}}
	}
	var out []checkResult
	if _, err := hub.ListBlobs(ctx); err != nil {
		return append(out, checkResult{Name: "hub reachable", Status: checkError, Detail: err.Error()})
	}
	out = append(out, checkResult{Name: "hub reachable", Status: checkOK, Detail: hubID})

	pushCursor, _ := store.HubCursor(ctx, "push:"+hubID)
	if pending, perr := store.LocalPendingEvents(ctx, pushCursor); perr == nil {
		if len(pending) > 0 {
			out = append(out, checkResult{Name: "pending push", Status: checkWarn, Detail: fmt.Sprintf("%d local event(s) not yet pushed", len(pending)), Remedy: "run `devstrap sync`"})
		} else {
			out = append(out, checkResult{Name: "pending push", Status: checkOK, Detail: "0"})
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
	return out
}

// runDoctorChecks collects all health checks into a graded result list (PROD-02).
func runDoctorChecks(ctx context.Context, opts *options) []checkResult {
	paths := opts.paths()
	var results []checkResult
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
	if _, err := os.Stat(paths.StateDB()); err == nil {
		store, err := opts.openState(ctx)
		if err != nil {
			results = append(results, checkResult{Name: "state database", Status: checkError, Detail: err.Error()})
		} else {
			defer closeStore(store)
			results = append(results, checkDB(ctx, store)...)
			results = append(results, checkSecretsRotation(ctx, store)...)
			results = append(results, checkDeviceKeys(ctx, paths, store)...)
			results = append(results, checkForgeCLIs(ctx, opts, store)...)
			results = append(results, checkBloblessCaveat(ctx, store)...)
		}
	} else if os.IsNotExist(err) {
		results = append(results, checkResult{Name: "state database", Status: checkWarn, Detail: "missing", Remedy: "run `devstrap init` (or doctor --fix)"})
	} else {
		results = append(results, checkResult{Name: "state database", Status: checkError, Detail: err.Error()})
	}
	results = append(results, checkRepoLocks(paths.Home)...)
	return results
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
		out = append(out, checkResult{Name: "sqlite quick_check", Status: checkError, Detail: err.Error(), Remedy: "restore from a `devstrap db backup`"})
	} else if check != "ok" {
		out = append(out, checkResult{Name: "sqlite quick_check", Status: checkError, Detail: check, Remedy: "restore from a `devstrap db backup`"})
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
		kind := ResolveForge(p.RemoteURL, "", p.ForgeKind, hostMap)
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

func checkDeviceKeys(ctx context.Context, paths config.Paths, store *state.Store) []checkResult {
	device, err := store.CurrentDevice(ctx)
	if err != nil {
		return []checkResult{{Name: "device key", Status: checkWarn, Detail: "no current device"}}
	}
	keyStore := devicekeys.NewHybridStore(paths.KeyDir(), platform.Detect().Keychain)
	var out []checkResult
	out = append(out, gradedKeyStatus("device key", deviceKeyStatus(ctx, keyStore, device.ID, device.PublicKey)))
	out = append(out, gradedKeyStatus("device signing key", deviceSigningKeyStatus(ctx, keyStore, device.ID, device.SigningPublicKey)))
	return out
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
