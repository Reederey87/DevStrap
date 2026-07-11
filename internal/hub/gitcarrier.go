// GitCarrierHub is the zero-infrastructure hub backend (AD-1 first slice): a
// private git repository — any host the user can already push to — carries the
// same zero-knowledge object set the R2/S3 backend stores (enc.v2 event
// ciphertext, age blobs, sealed snapshots, signed manifests/acks). No bucket,
// no credentials plane beyond the user's existing git auth.
//
// Architecture: rather than re-implementing the 24-method Hub contract, the
// carrier composes the ALREADY-PROVEN R2Hub semantics over a plain-filesystem
// S3Client (fsObjectStore) rooted in a local clone of the carrier repo, and
// adds the git transport around it:
//
//	read  = fetch + reset to the remote head, then delegate to R2Hub
//	write = fetch + reset, delegate to R2Hub (file mutations), one commit
//	        for the whole batch, push; on a non-fast-forward rejection
//	        refetch and re-apply with capped backoff (mutations are
//	        idempotent: every key is content-addressed or (device,seq)-unique)
//
// The atomic push-ref compare-and-swap is the linearization point that
// replaces S3 conditional PUT: If-None-Match/If-Match are evaluated against
// the freshly fetched head inside every attempt, so a lost push race
// re-evaluates them against the winner's state and surfaces the same
// ErrSweepLockHeld / ErrRetentionConflict outcomes as R2.
//
// Object LastModified (gc grace windows, sweep-lock TTL) cannot ride git
// commit times — a dedup re-put changes no bytes and history rewrites reset
// commit times — so fsObjectStore keeps an RFC3339Nano timestamp sidecar per
// object under .devstrap-meta/times/. Sidecars live OUTSIDE the workspaces/
// prefix, so no Hub listing ever sees them, and they travel with the tree
// through compaction squashes. The time is client-reported, which is
// acceptable for the advisory sweep lock's cooperating-clients contract
// (spec/15: a hostile writer with repo access defeats any dumb carrier).
//
// hub compact is the only history-bounding operation: after deleting cold
// event objects it rewrites the branch to a single parentless commit of the
// surviving tree and pushes with --force-with-lease, so the carrier's git
// history stops growing monotonically (deleting files never shrinks a repo);
// the host garbage-collects the unreachable objects. Concurrent pushers
// recover through their own fetch-and-reapply loop. The caller (hub compact)
// already holds the advisory sweep lock.
package hub

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Reederey87/DevStrap/internal/git"
	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
)

const (
	// gitPushAttempts bounds the optimistic non-fast-forward retry loop. Each
	// attempt refetches and re-applies, so attempts only repeat under live
	// contention from other devices.
	gitPushAttempts = 5
	// gitMarkerFile guards against clobbering a repository that is not a
	// DevStrap hub carrier: a non-empty branch without it is refused.
	gitMarkerFile = "devstrap-hub.json"
	// gitTimesPrefix is the timestamp-sidecar tree. It sorts outside every
	// workspaces/ listing prefix, so Hub enumeration never sees it.
	gitTimesPrefix = ".devstrap-meta/times/"
	// fsLockStale is when a same-machine cross-process lock (shared by the git
	// and folder carriers via fsLock) is considered abandoned when owner
	// liveness cannot be established. A live same-host holder is never broken,
	// regardless of mtime; legacy, corrupt, and cross-host records use this TTL.
	fsLockStale = 10 * time.Minute
	// fsLockHeartbeat is how often a live holder refreshes the lock mtime.
	fsLockHeartbeat = time.Minute
	// fsLockWait is how long a second local process waits for the lock.
	fsLockWait = 2 * time.Minute
)

// gitCarrierMarker is the content of gitMarkerFile.
type gitCarrierMarker struct {
	Version     int    `json:"version"`
	WorkspaceID string `json:"workspace_id"`
}

// gitHeadState is the persisted last-known-good carrier head (P7-HUB-02).
// The retention fields fingerprint the retention manifest as checked out at
// SHA; they let a non-compacting device distinguish a legitimate compaction
// squash (manifest strictly advanced, or byte-identical to this fingerprint —
// the parentless squash reuses the pre-squash manifest bytes) from a rewound
// carrier.
type gitHeadState struct {
	Version             int              `json:"version"` // 1
	SHA                 string           `json:"sha"`
	ObservedAt          string           `json:"observed_at"`                // RFC3339Nano
	RetentionSHA256     string           `json:"retention_sha256,omitempty"` // "" = no manifest at SHA
	RetentionProducedAt int64            `json:"retention_produced_at_hlc,omitempty"`
	RetentionFloors     map[string]int64 `json:"retention_floors,omitempty"`
}

// GitCarrierHub implements dssync.Hub over a private git repository.
// Construct with NewGitCarrierHub. Safe for concurrent use; all operations
// serialize on an in-process mutex plus a cross-process lock file, because
// they share one working clone.
type GitCarrierHub struct {
	remote      string
	branch      string
	workspaceID string
	dir         string // working clone (the carrier checkout)
	lockPath    string // cross-process lock, sibling of dir (never inside it)
	headPath    string // persisted verified head, sibling of dir (never reset)
	runner      git.Runner
	mu          sync.Mutex
	store       *fsObjectStore
	r2          R2Hub
	// fetchedSHA is the remote head observed by the last refresh; empty when
	// the remote branch does not exist yet. It is the --force-with-lease
	// expectation for the compaction squash.
	fetchedSHA string
	// sleep is a test seam for the backoff between push attempts.
	sleep func(time.Duration)
	// lockWait/lockHeartbeat are test seams for the cross-process lock timing.
	lockWait      time.Duration
	lockHeartbeat time.Duration
}

// Compile-time assertion that *GitCarrierHub satisfies dssync.Hub.
var _ dssync.Hub = (*GitCarrierHub)(nil)

// NewGitCarrierHub prepares a git-carrier hub for remote/branch. cacheRoot is
// the local clone cache directory (one subdirectory per remote+branch, so two
// hubs never share a checkout); it is created on demand. The remote must pass
// git.ValidateRemote; the branch must be a safe git branch name.
func NewGitCarrierHub(remote, branch, workspaceID, cacheRoot string) (*GitCarrierHub, error) {
	if err := git.ValidateRemote(remote); err != nil {
		return nil, fmt.Errorf("git hub remote: %w", err)
	}
	if branch == "" {
		branch = "main"
	}
	if !git.SafeBranchName(branch) {
		return nil, fmt.Errorf("git hub branch %q: not a safe branch name", branch)
	}
	if workspaceID == "" {
		return nil, errors.New("git hub: empty workspace id")
	}
	if cacheRoot == "" {
		return nil, errors.New("git hub: empty cache root")
	}
	sum := sha256.Sum256([]byte(remote + "\n" + branch))
	base := filepath.Join(cacheRoot, hex.EncodeToString(sum[:])[:16])
	dir := filepath.Join(base, "repo")
	store := &fsObjectStore{root: dir, obsPath: filepath.Join(base, "observed.json")}
	return &GitCarrierHub{
		remote:        remote,
		branch:        branch,
		workspaceID:   workspaceID,
		dir:           dir,
		lockPath:      filepath.Join(base, "repo.lock"),
		headPath:      filepath.Join(base, "head.json"),
		runner:        git.NewRunner(),
		store:         store,
		r2:            R2Hub{S3: store, WorkspaceID: workspaceID},
		sleep:         time.Sleep,
		lockWait:      fsLockWait,
		lockHeartbeat: fsLockHeartbeat,
	}, nil
}

func (g *GitCarrierHub) headStateError(detail string, err error) error {
	base := filepath.Dir(g.dir)
	if err != nil {
		return fmt.Errorf("git hub head state %s: %s: %w; remove the carrier cache to re-adopt the remote: rm -rf %s", g.headPath, detail, err, base)
	}
	return fmt.Errorf("git hub head state %s: %s; remove the carrier cache to re-adopt the remote: rm -rf %s", g.headPath, detail, base)
}

// loadHeadState reads the durable last verified carrier head. Corruption is
// abnormal because writes are atomic, so it fails closed instead of silently
// granting a new trust-on-first-use window.
func (g *GitCarrierHub) loadHeadState() (gitHeadState, bool, error) {
	raw, err := os.ReadFile(g.headPath) //nolint:gosec // constructor-fixed path under the carrier cache
	if errors.Is(err, os.ErrNotExist) {
		return gitHeadState{}, false, nil
	}
	if err != nil {
		return gitHeadState{}, false, g.headStateError("cannot read persisted state", err)
	}
	var state gitHeadState
	if err := json.Unmarshal(raw, &state); err != nil {
		return gitHeadState{}, false, g.headStateError("cannot parse persisted state", err)
	}
	if state.Version != 1 {
		return gitHeadState{}, false, g.headStateError(fmt.Sprintf("version %d is unsupported (want 1)", state.Version), nil)
	}
	if len(strings.TrimSpace(state.SHA)) < 8 {
		return gitHeadState{}, false, g.headStateError("contains a missing or short sha", nil)
	}
	return state, true, nil
}

// saveHeadStateLocked fingerprints the retention head directly through the
// inner store. Calling g.GetRetention here would re-enter readRefresh while
// g.mu is held and deadlock.
func (g *GitCarrierHub) saveHeadStateLocked(ctx context.Context, sha string) error {
	state := gitHeadState{
		Version:    1,
		SHA:        strings.TrimSpace(sha),
		ObservedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	raw, _, err := g.r2.GetRetention(ctx)
	switch {
	case errors.Is(err, dssync.ErrRetentionNotFound):
		// No manifest at this head: the empty fingerprint is meaningful.
	case err != nil:
		return fmt.Errorf("read git hub retention for head state: %w", err)
	default:
		sum := sha256.Sum256(raw)
		state.RetentionSHA256 = hex.EncodeToString(sum[:])
		if manifest, perr := dssync.ParseRetentionManifest(raw); perr == nil {
			state.RetentionProducedAt = manifest.ProducedAt
			state.RetentionFloors = manifest.Floors
		}
	}
	raw, err = json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshal git hub head state: %w", err)
	}
	if err := writeFileAtomic(g.headPath, raw); err != nil {
		return fmt.Errorf("save git hub head state %s: %w", g.headPath, err)
	}
	return nil
}

func commandExitCode(err error) (int, bool) {
	var exitErr interface{ ExitCode() int }
	if !errors.As(err, &exitErr) {
		return 0, false
	}
	return exitErr.ExitCode(), true
}

// checkHeadContinuityLocked refuses a carrier rewind unless the checked-out
// tree plausibly represents hub compaction. Production compact publishes the
// advanced manifest on a NORMAL commit first and the parentless squash keeps
// those same manifest bytes, so a device that observed the advanced tip sees
// the squash with an IDENTICAL fingerprint (not a strictly newer one) — both
// shapes are accepted. This is only a transport-level plausibility gate: the
// sync layer still verifies the retention manifest signature against pinned
// approved devices.
func (g *GitCarrierHub) checkHeadContinuityLocked(ctx context.Context, newSHA string) error {
	last, ok, err := g.loadHeadState()
	if err != nil || !ok {
		return err
	}
	newSHA = strings.TrimSpace(newSHA)
	if newSHA == last.SHA {
		return nil
	}

	oldKnown := true
	if _, err := g.runner.Run(ctx, g.dir, "cat-file", "-e", last.SHA+"^{commit}"); err != nil {
		code, hasCode := commandExitCode(err)
		// cat-file uses 128 for a syntactically valid object name that is not
		// present in this object database (fresh clone / host-side GC).
		if hasCode && code == 128 && (strings.Contains(strings.ToLower(err.Error()), "not a valid object") || strings.Contains(strings.ToLower(err.Error()), "bad object")) {
			oldKnown = false
		} else {
			return fmt.Errorf("inspect last verified git hub head: %w", err)
		}
	}
	if oldKnown {
		if _, err := g.runner.Run(ctx, g.dir, "merge-base", "--is-ancestor", last.SHA, newSHA); err == nil {
			return nil
		} else if code, ok := commandExitCode(err); !ok || code != 1 {
			return fmt.Errorf("check git hub head ancestry: %w", err)
		}
	}

	advanced := false
	var newFloors map[string]int64
	haveNewFloors := false
	raw, _, rerr := g.r2.GetRetention(ctx)
	if rerr == nil {
		if manifest, perr := dssync.ParseRetentionManifest(raw); perr == nil {
			newFloors = manifest.Floors
			haveNewFloors = true
			switch {
			case last.RetentionSHA256 == "":
				// First-ever manifest observation (TOFU residual, spec/15).
				advanced = true
			case last.RetentionProducedAt == 0:
				// The recorded fingerprint never parsed, so only identical
				// bytes are judgeable (below).
			case manifest.ProducedAt > last.RetentionProducedAt:
				advanced = true
				for dev, floor := range last.RetentionFloors {
					if manifest.Floors[dev] < floor {
						advanced = false
						break
					}
				}
			}
		}
		if !advanced && last.RetentionSHA256 != "" {
			// The squash of exactly the retention state this device last
			// verified: compact's parentless commit reuses the pre-squash
			// manifest bytes. Also self-heals a compactor that crashed after
			// its force-push but before persisting the squashed head.
			sum := sha256.Sum256(raw)
			advanced = hex.EncodeToString(sum[:]) == last.RetentionSHA256
		}
	} else if !errors.Is(rerr, dssync.ErrRetentionNotFound) {
		return fmt.Errorf("read git hub retention for continuity check: %w", rerr)
	}
	if advanced && oldKnown {
		// Content gate: a legitimate squash only deletes events BELOW the new
		// floors. A rewrite that drops an at-or-above-floor event object is a
		// data-losing rewind no manifest can explain, whatever it claims.
		if err := g.checkCompactionDeletesLocked(ctx, last.SHA, newSHA, newFloors, haveNewFloors); err != nil {
			return err
		}
	}
	if advanced {
		return nil
	}
	return fmt.Errorf("git hub %s: carrier history was rewritten — branch %s head %.8s is not a descendant of the last verified head %.8s and carries no advanced retention manifest; refusing to sync. If another trusted device confirms the carrier is correct (run devstrap status / devstrap sync there), re-adopt it by removing this device's carrier cache: rm -rf %s. Otherwise restore the carrier repository from the host's backup.", redactedRemote(g.remote), g.branch, newSHA, last.SHA, filepath.Dir(g.dir)) //nolint:staticcheck // P7-HUB-02 specifies this multi-sentence user-facing refusal verbatim.
}

// checkCompactionDeletesLocked verifies that every event object deleted between
// the last verified head and the new head sits BELOW the new retention floors —
// the only deletions `hub compact` performs. Event keys are the current
// workspaces/<ws>/eventlog/<device>/<seq pad20>_<id>.json layout plus the
// retired dual-read workspaces/<ws>/events/<hlc>/<device>/<seq>/<id>.json.
func (g *GitCarrierHub) checkCompactionDeletesLocked(ctx context.Context, lastSHA, newSHA string, floors map[string]int64, haveFloors bool) error {
	prefix := fmt.Sprintf("workspaces/%s/eventlog/", g.workspaceID)
	legacyPrefix := fmt.Sprintf("workspaces/%s/events/", g.workspaceID)
	out, err := g.runner.Run(ctx, g.dir, "diff", "--name-only", "--diff-filter=D", lastSHA, newSHA, "--", prefix, legacyPrefix)
	if err != nil {
		return fmt.Errorf("diff git hub event objects for continuity check: %w", err)
	}
	refuse := func(path string) error {
		return fmt.Errorf("git hub %s: carrier history was rewritten — head %.8s deletes event object %s at or above the retention floors, which no compaction produces; refusing to sync. If another trusted device confirms the carrier is correct (run devstrap status / devstrap sync there), re-adopt it by removing this device's carrier cache: rm -rf %s. Otherwise restore the carrier repository from the host's backup.", redactedRemote(g.remote), newSHA, path, filepath.Dir(g.dir)) //nolint:staticcheck // P7-HUB-02 specifies this multi-sentence user-facing refusal verbatim.
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var device, seqText string
		switch {
		case strings.HasPrefix(line, prefix): // <device>/<seq pad20>_<event_id>.json
			parts := strings.Split(strings.TrimPrefix(line, prefix), "/")
			if len(parts) != 2 {
				return refuse(line) // unrecognizable event key shape: fail closed
			}
			device = parts[0]
			seqText, _, _ = strings.Cut(parts[1], "_")
		case strings.HasPrefix(line, legacyPrefix): // <hlc>/<device>/<seq>/<event_id>.json
			parts := strings.Split(strings.TrimPrefix(line, legacyPrefix), "/")
			if len(parts) != 4 {
				return refuse(line)
			}
			device, seqText = parts[1], parts[2]
		default:
			return refuse(line)
		}
		if !haveFloors {
			return refuse(line) // events deleted with no floors to justify it
		}
		floor, ok := floors[device]
		if !ok {
			return refuse(line)
		}
		seq, perr := strconv.ParseInt(seqText, 10, 64)
		if perr != nil || seq >= floor {
			return refuse(line)
		}
	}
	return nil
}

// lock takes the in-process mutex plus the cross-process lock file and returns
// the release func. The lock file lives OUTSIDE the checkout so git clean can
// never remove a held lock. While held, a heartbeat goroutine refreshes the
// lock file's mtime every lockHeartbeat, so the gitLockStale breaker can only
// ever fire on a DEAD holder — a live process blocked in an hour-long fetch
// keeps its lock warm and can never have the shared checkout stolen and reset
// underneath it.
func (g *GitCarrierHub) lock() (func(), error) {
	return fsLock{
		mu:        &g.mu,
		path:      g.lockPath,
		wait:      g.lockWait,
		heartbeat: g.lockHeartbeat,
		stale:     fsLockStale,
		sleep:     g.sleep,
	}.acquire()
}

// ensureRepoLocked initializes the working clone on first use: a plain `git
// init` plus `remote add origin`, never `git clone`, so an empty (just
// created) carrier repository needs no special case — the first fetch simply
// reports the branch as absent and the first push creates it.
func (g *GitCarrierHub) ensureRepoLocked(ctx context.Context) error {
	if _, err := os.Stat(filepath.Join(g.dir, ".git")); err == nil {
		return nil
	}
	if err := os.MkdirAll(g.dir, 0o700); err != nil {
		return fmt.Errorf("create git hub clone dir: %w", err)
	}
	if _, err := g.runner.Run(ctx, g.dir, "init", "--quiet", "--initial-branch", g.branch); err != nil {
		return fmt.Errorf("init git hub clone: %w", err)
	}
	if _, err := g.runner.Run(ctx, g.dir, "remote", "add", "origin", g.remote); err != nil {
		return fmt.Errorf("wire git hub remote: %w", err)
	}
	return nil
}

// refreshLocked synchronizes the working clone with the remote branch head:
// fetch, hard-reset, clean, then validate the carrier marker. An absent remote
// branch (brand-new carrier repo) resets the clone to an empty unborn state.
func (g *GitCarrierHub) refreshLocked(ctx context.Context) error {
	if err := g.ensureRepoLocked(ctx); err != nil {
		return err
	}
	// Runner.Fetch rides the shared transient-network retry + the
	// long-transfer deadline class, so a flaky link degrades to a retry
	// instead of failing the whole sync cycle (matching R2's retry posture).
	err := g.runner.Fetch(ctx, g.dir, "origin", g.branch)
	switch {
	case err == nil:
		sha, err := g.runner.Run(ctx, g.dir, "rev-parse", "FETCH_HEAD")
		if err != nil {
			return fmt.Errorf("resolve git hub head: %w", err)
		}
		if _, err := g.runner.Run(ctx, g.dir, "reset", "--hard", "--quiet", sha); err != nil {
			return fmt.Errorf("reset git hub clone: %w", err)
		}
		if _, err := g.runner.Run(ctx, g.dir, "clean", "-fdq"); err != nil {
			return fmt.Errorf("clean git hub clone: %w", err)
		}
		if err := g.validateMarkerLocked(); err != nil {
			return err
		}
		if err := g.checkHeadContinuityLocked(ctx, sha); err != nil {
			return err
		}
		if err := g.saveHeadStateLocked(ctx, sha); err != nil {
			return err
		}
		g.fetchedSHA = sha
		return nil
	case errors.Is(err, git.ErrBranchNotFound):
		last, ok, lerr := g.loadHeadState()
		if lerr != nil {
			return lerr
		}
		if ok {
			return fmt.Errorf("git hub %s: branch %s no longer exists but this device previously verified head %.8s — the carrier was deleted or rewound; refusing to re-found it. Recreate/restore the carrier repo, or verify from another trusted device and rm -rf %s to re-initialize.", redactedRemote(g.remote), g.branch, last.SHA, filepath.Dir(g.dir)) //nolint:staticcheck // P7-HUB-02 specifies this multi-sentence user-facing refusal verbatim.
		}
		// Empty carrier: reset to an unborn branch with an empty tree so hub
		// state is exactly the remote state (nothing durable lives locally).
		if _, serr := g.runner.Run(ctx, g.dir, "symbolic-ref", "HEAD", "refs/heads/"+g.branch); serr != nil {
			return fmt.Errorf("reset git hub head: %w", serr)
		}
		_, _ = g.runner.Run(ctx, g.dir, "update-ref", "-d", "refs/heads/"+g.branch)
		if _, cerr := g.runner.Run(ctx, g.dir, "read-tree", "--empty"); cerr != nil {
			return fmt.Errorf("empty git hub index: %w", cerr)
		}
		if err := clearDirExceptGit(g.dir); err != nil {
			return fmt.Errorf("empty git hub clone: %w", err)
		}
		g.fetchedSHA = ""
		return nil
	default:
		return fmt.Errorf("fetch git hub: %w", err)
	}
}

// validateMarkerLocked enforces the carrier marker on a non-empty branch: a
// tree without it is some OTHER repository and is refused rather than written
// to; a marker for a different workspace is refused rather than mixed. A
// symlinked marker is refused outright — reading or later rewriting it must
// never follow a hostile tree outside the checkout.
func (g *GitCarrierHub) validateMarkerLocked() error {
	root, err := os.OpenRoot(g.dir)
	if err != nil {
		return fmt.Errorf("open git hub root: %w", err)
	}
	defer func() { _ = root.Close() }()
	if info, lerr := root.Lstat(gitMarkerFile); lerr == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("git hub %s: %s is a symlink; refusing", redactedRemote(g.remote), gitMarkerFile)
	}
	raw, err := root.ReadFile(gitMarkerFile)
	if errors.Is(err, os.ErrNotExist) {
		empty, eerr := dirEmptyExceptGit(g.dir)
		if eerr != nil {
			return eerr
		}
		if empty {
			return nil // freshly created branch content lands with the first push
		}
		return fmt.Errorf("git hub %s: branch %q has content but no %s marker; refusing to use a non-hub repository as a hub carrier", redactedRemote(g.remote), g.branch, gitMarkerFile)
	}
	if err != nil {
		return fmt.Errorf("read git hub marker: %w", err)
	}
	var marker gitCarrierMarker
	if err := json.Unmarshal(raw, &marker); err != nil {
		return fmt.Errorf("parse git hub marker: %w", err)
	}
	if marker.Version != 1 {
		return fmt.Errorf("git hub marker version %d: this devstrap understands version 1 (upgrade devstrap?)", marker.Version)
	}
	if marker.WorkspaceID != g.workspaceID {
		return fmt.Errorf("git hub carrier belongs to workspace %s, not %s; point 'hub:' at this workspace's carrier repo", marker.WorkspaceID, g.workspaceID)
	}
	return nil
}

// writeMarkerLocked seeds the carrier marker on a bootstrap write. It rides
// the same os.Root confinement as every other checkout write so its safety is
// structural, not dependent on a validateMarkerLocked having run first
// (P7-SEC-04 review).
func (g *GitCarrierHub) writeMarkerLocked() error {
	root, err := os.OpenRoot(g.dir)
	if err != nil {
		return fmt.Errorf("open git hub root: %w", err)
	}
	defer func() { _ = root.Close() }()
	if _, err := root.Lstat(gitMarkerFile); err == nil {
		return nil
	}
	raw, err := json.Marshal(gitCarrierMarker{Version: 1, WorkspaceID: g.workspaceID})
	if err != nil {
		return err
	}
	f, err := root.OpenFile(gitMarkerFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create git hub marker: %w", err)
	}
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		return fmt.Errorf("write git hub marker: %w", err)
	}
	return f.Close()
}

// commitLocked stages everything and commits when the tree changed. The
// sanitized git environment has no user identity (GIT_CONFIG_GLOBAL is
// /dev/null), so the committer is pinned explicitly.
func (g *GitCarrierHub) commitLocked(ctx context.Context, message string) (changed bool, err error) {
	if _, err := g.runner.Run(ctx, g.dir, "add", "-A"); err != nil {
		return false, fmt.Errorf("stage git hub changes: %w", err)
	}
	status, err := g.runner.Run(ctx, g.dir, "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("read git hub status: %w", err)
	}
	if strings.TrimSpace(status) == "" {
		return false, nil
	}
	if _, err := g.runner.Run(ctx, g.dir,
		"-c", "user.name=devstrap", "-c", "user.email=devstrap@localhost",
		"commit", "--quiet", "-m", message); err != nil {
		return false, fmt.Errorf("commit git hub changes: %w", err)
	}
	return true, nil
}

// writeLoop is the optimistic write cycle shared by every mutating Hub method:
// refresh → apply (idempotent file mutations via the composed R2Hub) → seed
// the marker → one commit → push; a non-fast-forward push refetches and
// re-applies with capped backoff. Errors from apply are terminal — including
// the conditional-put outcomes (ErrSweepLockHeld, ErrRetentionConflict), which
// re-evaluate against the winner's state after a lost race.
func (g *GitCarrierHub) writeLoop(ctx context.Context, op string, apply func() error) error {
	release, err := g.lock()
	if err != nil {
		return err
	}
	defer release()
	backoff := 100 * time.Millisecond
	for attempt := 0; attempt < gitPushAttempts; attempt++ {
		if err := g.refreshLocked(ctx); err != nil {
			return err
		}
		if err := apply(); err != nil {
			return err
		}
		if err := g.writeMarkerLocked(); err != nil {
			return err
		}
		changed, err := g.commitLocked(ctx, "devstrap hub "+op)
		if err != nil {
			return err
		}
		if !changed {
			return nil
		}
		// PushBranch rides the long-transfer deadline; repeating a failed
		// push is safe here (idempotent mutations + the ref CAS), so both a
		// lost race (non-fast-forward) and a transient network error retry
		// through the same refetch-and-reapply cycle.
		err = g.runner.PushBranch(ctx, g.dir, "origin", g.branch)
		if err == nil {
			headSHA, rerr := g.runner.Run(ctx, g.dir, "rev-parse", "HEAD")
			if rerr != nil {
				return fmt.Errorf("resolve pushed git hub head: %w", rerr)
			}
			if rerr := g.saveHeadStateLocked(ctx, headSHA); rerr != nil {
				return rerr
			}
			return nil
		}
		if !errors.Is(err, git.ErrNonFastForward) && !errors.Is(err, git.ErrNetwork) {
			return fmt.Errorf("push git hub: %w", err)
		}
		g.sleep(backoff)
		if backoff < 2*time.Second {
			backoff *= 2
		}
	}
	return fmt.Errorf("push git hub: %w after %d attempts (live contention from other devices?)", git.ErrNonFastForward, gitPushAttempts)
}

// readRefresh refreshes the clone and runs fn against the up-to-date checkout
// under the lock (reads share the clone with writers).
func (g *GitCarrierHub) readRefresh(ctx context.Context, fn func() error) error {
	release, err := g.lock()
	if err != nil {
		return err
	}
	defer release()
	if err := g.refreshLocked(ctx); err != nil {
		return err
	}
	return fn()
}

// --- dssync.Hub: event plane ---

func (g *GitCarrierHub) Push(ctx context.Context, events []state.Event) error {
	return g.writeLoop(ctx, "push", func() error { return g.r2.Push(ctx, events) })
}

func (g *GitCarrierHub) Pull(ctx context.Context, after dssync.Cursor) ([]state.Event, error) {
	var events []state.Event
	err := g.readRefresh(ctx, func() (ferr error) {
		events, ferr = g.r2.Pull(ctx, after)
		return ferr
	})
	return events, err
}

func (g *GitCarrierHub) DeleteDeviceStream(ctx context.Context, deviceID string) (int, error) {
	var deleted int
	err := g.writeLoop(ctx, "revoke", func() (ferr error) {
		deleted, ferr = g.r2.DeleteDeviceStream(ctx, deviceID)
		return ferr
	})
	return deleted, err
}

// MigrateLegacyEvents is a structural no-op: the git carrier never had the
// retired HLC-keyed layout. Delegated for symmetric reporting. A dry run goes
// through the READ path — the write loop would seed the carrier marker (and,
// on an empty carrier, create the branch), violating the CLI's
// report-without-writing dry-run contract.
func (g *GitCarrierHub) MigrateLegacyEvents(ctx context.Context, dryRun bool) (int, int, error) {
	var migrated, kept int
	if dryRun {
		err := g.readRefresh(ctx, func() (ferr error) {
			migrated, kept, ferr = g.r2.MigrateLegacyEvents(ctx, true)
			return ferr
		})
		return migrated, kept, err
	}
	err := g.writeLoop(ctx, "migrate-events", func() (ferr error) {
		migrated, kept, ferr = g.r2.MigrateLegacyEvents(ctx, false)
		return ferr
	})
	return migrated, kept, err
}

// HasEvents reports whether any event was ever recorded on this hub (the
// doctor --remote capability probe, mirroring FileHub.HasEvents).
func (g *GitCarrierHub) HasEvents(ctx context.Context) (bool, error) {
	var has bool
	err := g.readRefresh(ctx, func() error {
		objs, _, lerr := g.store.ListObjectsV2(ctx, fmt.Sprintf("workspaces/%s/eventlog/", g.workspaceID), "", 1)
		if lerr != nil {
			return lerr
		}
		has = len(objs) > 0
		return nil
	})
	return has, err
}

// --- dssync.Hub: blob plane ---

func (g *GitCarrierHub) PutBlob(ctx context.Context, sha256Hex string, r io.Reader) error {
	// Read once outside the loop: the reader cannot be rewound between
	// non-fast-forward retries.
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("read blob: %w", err)
	}
	return g.writeLoop(ctx, "put-blob", func() error { return g.r2.PutBlob(ctx, sha256Hex, bytes.NewReader(data)) })
}

func (g *GitCarrierHub) GetBlob(ctx context.Context, sha256Hex string) (io.ReadCloser, error) {
	var rc io.ReadCloser
	err := g.readRefresh(ctx, func() (ferr error) {
		rc, ferr = g.r2.GetBlob(ctx, sha256Hex)
		return ferr
	})
	return rc, err
}

func (g *GitCarrierHub) DeleteBlob(ctx context.Context, sha256Hex string) error {
	return g.writeLoop(ctx, "gc", func() error { return g.r2.DeleteBlob(ctx, sha256Hex) })
}

func (g *GitCarrierHub) ListBlobs(ctx context.Context) ([]dssync.BlobInfo, error) {
	var infos []dssync.BlobInfo
	err := g.readRefresh(ctx, func() (ferr error) {
		infos, ferr = g.r2.ListBlobs(ctx)
		return ferr
	})
	return infos, err
}

func (g *GitCarrierHub) StatBlob(ctx context.Context, sha256Hex string) (dssync.BlobInfo, error) {
	var info dssync.BlobInfo
	err := g.readRefresh(ctx, func() (ferr error) {
		info, ferr = g.r2.StatBlob(ctx, sha256Hex)
		return ferr
	})
	return info, err
}

// --- dssync.Hub: retention manifest (CAS head object) ---

func (g *GitCarrierHub) GetRetention(ctx context.Context) ([]byte, string, error) {
	var (
		raw  []byte
		etag string
	)
	err := g.readRefresh(ctx, func() (ferr error) {
		raw, etag, ferr = g.r2.GetRetention(ctx)
		return ferr
	})
	return raw, etag, err
}

func (g *GitCarrierHub) PutRetention(ctx context.Context, raw []byte, ifMatchETag string) error {
	return g.writeLoop(ctx, "retention", func() error { return g.r2.PutRetention(ctx, raw, ifMatchETag) })
}

// --- dssync.Hub: sealed snapshot objects ---

func (g *GitCarrierHub) PutSnapshotObject(ctx context.Context, sha256Hex string, body []byte) error {
	return g.writeLoop(ctx, "snapshot", func() error { return g.r2.PutSnapshotObject(ctx, sha256Hex, body) })
}

func (g *GitCarrierHub) GetSnapshotObject(ctx context.Context, sha256Hex string) ([]byte, error) {
	var body []byte
	err := g.readRefresh(ctx, func() (ferr error) {
		body, ferr = g.r2.GetSnapshotObject(ctx, sha256Hex)
		return ferr
	})
	return body, err
}

func (g *GitCarrierHub) ListSnapshotObjects(ctx context.Context) ([]dssync.BlobInfo, error) {
	var infos []dssync.BlobInfo
	err := g.readRefresh(ctx, func() (ferr error) {
		infos, ferr = g.r2.ListSnapshotObjects(ctx)
		return ferr
	})
	return infos, err
}

func (g *GitCarrierHub) DeleteSnapshotObject(ctx context.Context, sha256Hex string) error {
	return g.writeLoop(ctx, "snapshot-gc", func() error { return g.r2.DeleteSnapshotObject(ctx, sha256Hex) })
}

// --- dssync.Hub: signed per-device sync acks ---

func (g *GitCarrierHub) PutAck(ctx context.Context, deviceID string, raw []byte) error {
	return g.writeLoop(ctx, "ack", func() error { return g.r2.PutAck(ctx, deviceID, raw) })
}

func (g *GitCarrierHub) ListAcks(ctx context.Context) (map[string][]byte, error) {
	var acks map[string][]byte
	err := g.readRefresh(ctx, func() (ferr error) {
		acks, ferr = g.r2.ListAcks(ctx)
		return ferr
	})
	return acks, err
}

func (g *GitCarrierHub) DeleteAck(ctx context.Context, deviceID string) error {
	return g.writeLoop(ctx, "ack-gc", func() error { return g.r2.DeleteAck(ctx, deviceID) })
}

// --- dssync.Hub: advisory sweep lock ---

func (g *GitCarrierHub) GetSweepLock(ctx context.Context) ([]byte, time.Time, error) {
	var (
		raw []byte
		mod time.Time
	)
	err := g.readRefresh(ctx, func() (ferr error) {
		raw, mod, ferr = g.r2.GetSweepLock(ctx)
		return ferr
	})
	// Clamp the lock's age DOWN to this clone's observation floor: a
	// future-dated sidecar (skewed or hostile holder clock) must not make a
	// dead holder's lock unbreakable — once THIS reader has watched the lock
	// for a full TTL, it is stale regardless of its self-reported time. The
	// opposite direction (modTime's max-floor) intentionally does not apply
	// here: it protects freshness for gc, while the breaker needs age.
	if err == nil {
		if obs, ok := g.store.observedAt(fmt.Sprintf("workspaces/%s/meta/sweep.lock", g.workspaceID)); ok && obs.Before(mod) {
			mod = obs
		}
	}
	return raw, mod, err
}

func (g *GitCarrierHub) PutSweepLock(ctx context.Context, raw []byte) error {
	return g.writeLoop(ctx, "sweep-lock", func() error { return g.r2.PutSweepLock(ctx, raw) })
}

func (g *GitCarrierHub) DeleteSweepLock(ctx context.Context) error {
	return g.writeLoop(ctx, "sweep-unlock", func() error { return g.r2.DeleteSweepLock(ctx) })
}

// --- dssync.Hub: event-log compaction (the history-bounding squash) ---

// CompactEventsBelow deletes cold event objects like R2, then — because file
// deletion never shrinks a git repository — rewrites the branch to a single
// parentless commit of the surviving tree and pushes it with
// --force-with-lease against the head this pass fetched. The caller holds the
// advisory sweep lock; a concurrent pusher that loses the lease race simply
// refetches the squashed head and re-applies (its mutations are idempotent).
// A lost lease HERE refetches and re-runs the deletion against the new head.
func (g *GitCarrierHub) CompactEventsBelow(ctx context.Context, floors dssync.Cursor) (int, error) {
	release, err := g.lock()
	if err != nil {
		return 0, err
	}
	defer release()
	backoff := 100 * time.Millisecond
	var deleted int
	for attempt := 0; attempt < gitPushAttempts; attempt++ {
		if err := g.refreshLocked(ctx); err != nil {
			return 0, err
		}
		deleted, err = g.r2.CompactEventsBelow(ctx, floors)
		if err != nil {
			return 0, err
		}
		if g.fetchedSHA == "" {
			return deleted, nil // empty carrier: nothing to squash or push
		}
		if err := g.writeMarkerLocked(); err != nil {
			return 0, err
		}
		if _, err := g.runner.Run(ctx, g.dir, "add", "-A"); err != nil {
			return 0, fmt.Errorf("stage git hub compaction: %w", err)
		}
		tree, err := g.runner.Run(ctx, g.dir, "write-tree")
		if err != nil {
			return 0, fmt.Errorf("write git hub compaction tree: %w", err)
		}
		newSHA, err := g.runner.Run(ctx, g.dir,
			"-c", "user.name=devstrap", "-c", "user.email=devstrap@localhost",
			"commit-tree", tree, "-m", "devstrap hub compact")
		if err != nil {
			return 0, fmt.Errorf("build git hub compaction commit: %w", err)
		}
		_, err = g.runner.Run(ctx, g.dir, "push", "--quiet",
			"--force-with-lease=refs/heads/"+g.branch+":"+g.fetchedSHA,
			"origin", newSHA+":refs/heads/"+g.branch)
		if err == nil {
			if _, rerr := g.runner.Run(ctx, g.dir, "reset", "--hard", "--quiet", newSHA); rerr != nil {
				return deleted, fmt.Errorf("align git hub clone after compaction: %w", rerr)
			}
			if rerr := g.saveHeadStateLocked(ctx, newSHA); rerr != nil {
				return deleted, rerr
			}
			g.fetchedSHA = newSHA
			return deleted, nil
		}
		if !errors.Is(err, git.ErrNonFastForward) && !errors.Is(err, git.ErrNetwork) {
			return 0, fmt.Errorf("push git hub compaction: %w", err)
		}
		g.sleep(backoff)
		if backoff < 2*time.Second {
			backoff *= 2
		}
	}
	return 0, fmt.Errorf("push git hub compaction: %w after %d attempts", git.ErrNonFastForward, gitPushAttempts)
}

// --- fsObjectStore: the plain-filesystem S3Client over the checkout ---

// fsObjectStore satisfies S3Client against the carrier checkout, mirroring the
// memS3 conformance double's semantics exactly. Every write also rewrites the
// object's timestamp sidecar (.devstrap-meta/times/<key>) so LastModified has
// R2's freshness behavior: an unconditional dedup re-put refreshes the time
// even though the object bytes are unchanged — and the changed sidecar is what
// makes the write loop commit and propagate that refresh.
//
// Sidecar times are WRITER-reported, and destructive age decisions (the gc
// grace window) must not trust a skewed writer: a two-days-slow device would
// otherwise upload a blob whose sidecar already looks past the grace window,
// and another device's `hub gc` could delete it before its referencing event
// lands. The store therefore keeps a per-clone OBSERVATION FLOOR
// (observed.json beside the clone, never inside the repo): the first time this
// clone sees a key it records its own clock, and every reported LastModified
// is floored at that observation — no object can ever look older to a reader
// than the reader has known about it, so "younger than the grace window" is
// judged against the reader's clock, exactly like R2's server time. The cost
// is that a fresh clone sees everything as newly-observed and its gc keeps
// everything for one extra grace window — fail-safe by construction.
type fsObjectStore struct {
	root string
	// rootInfo pins a shared folder carrier to the directory registered at
	// construction. A Root opened after guard-time revalidation is compared
	// against it, closing a swap between the path check and OpenRoot.
	// Git carriers leave this nil because their checkout is private and reset
	// under the carrier lock before every operation batch.
	rootInfo fs.FileInfo
	// obsPath is the per-clone observation index (key -> RFC3339Nano first
	// seen by THIS clone). Outside the checkout: never committed, never reset.
	obsPath string
	// wmu guards concurrent writers within one process (R2Hub's push/pull
	// fan-out calls S3Client methods concurrently; distinct keys write
	// distinct files, but MkdirAll+WriteFile pairs stay simplest serialized).
	// It also guards the observation index.
	wmu sync.Mutex
	obs map[string]time.Time
}

func (s *fsObjectStore) keyPath(key string) (string, error) {
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "\\") {
		return "", fmt.Errorf("%w: %q", dssync.ErrInvalidBlobKey, key)
	}
	for _, part := range strings.Split(key, "/") {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("%w: %q", dssync.ErrInvalidBlobKey, key)
		}
	}
	return key, nil
}

func (s *fsObjectStore) openRoot() (*os.Root, error) {
	root, err := os.OpenRoot(s.root)
	if err != nil {
		return nil, fmt.Errorf("open hub object root: %w", err)
	}
	if s.rootInfo != nil {
		info, serr := root.Stat(".")
		if serr != nil {
			_ = root.Close()
			return nil, fmt.Errorf("stat hub object root: %w", serr)
		}
		if !os.SameFile(s.rootInfo, info) {
			_ = root.Close()
			return nil, fmt.Errorf("folder hub: root %q no longer denotes the registered directory; refusing to operate outside it", s.root)
		}
	}
	return root, nil
}

func timePath(key string) string {
	return gitTimesPrefix + key
}

func (s *fsObjectStore) writeTimestamp(root *os.Root, key string) error {
	name := timePath(key)
	if err := root.MkdirAll(path.Dir(name), 0o700); err != nil {
		return err
	}
	// Sidecars live in the shared folder for the folder carrier; use the same
	// confined atomic write as object payloads (P7-HUB-05/P7-SEC-04).
	return writeRootFileAtomic(root, name, []byte(time.Now().UTC().Format(time.RFC3339Nano)))
}

// writeRootFileAtomic publishes a complete root-relative object using a
// same-directory temp, fsync, and Root.Rename. Rename is filesystem-atomic but
// cannot eliminate a cloud drive's mid-replication window (P7-HUB-05).
func writeRootFileAtomic(root *os.Root, name string, body []byte) error {
	dir := path.Dir(name)
	var tmp *os.File
	var tmpPath string
	for i := 0; i < 100; i++ {
		var suffix [8]byte
		if _, err := io.ReadFull(rand.Reader, suffix[:]); err != nil {
			return fmt.Errorf("name hub object temp: %w", err)
		}
		tmpPath = path.Join(dir, ".tmp-"+hex.EncodeToString(suffix[:]))
		var err error
		tmp, err = root.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("create hub object temp: %w", err)
		}
	}
	if tmp == nil {
		return errors.New("create hub object temp: exhausted unique names")
	}
	defer func() { _ = root.Remove(tmpPath) }()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write hub object temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("fsync hub object temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close hub object temp: %w", err)
	}
	if err := root.Rename(tmpPath, name); err != nil {
		return fmt.Errorf("rename hub object: %w", err)
	}
	if dirFile, err := root.Open(dir); err == nil {
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}
	return nil
}

// staleTempAge is how old an orphan object-write temp must be before listKeys
// reclaims it (crash between create and rename).
const staleTempAge = time.Hour

// writeFileAtomic writes a private-cache file outside the object-store root
// (notably head.json) via a same-directory temp file, fsync, and rename.
// This is deliberately separate from writeRootFileAtomic: private cache files
// live beside the clone, outside the confined shared object tree.
func writeFileAtomic(path string, body []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create hub object temp: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod hub object temp: %w", err)
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write hub object temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("fsync hub object temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close hub object temp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename hub object: %w", err)
	}
	// Fsync the directory so the rename's directory-entry update survives a
	// power loss immediately after this call returns; without it a crash can
	// revert the directory entry to the prior (still-complete, never torn)
	// generation. Best-effort: some filesystems/platforms don't support
	// directory fsync, so a failure here is not surfaced as a write error.
	if dirFile, err := os.Open(dir); err == nil { //nolint:gosec // private-cache helper; object-store writes use writeRootFileAtomic
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}
	return nil
}

func (s *fsObjectStore) modTime(root *os.Root, key string) time.Time {
	sidecar := time.Time{}
	if raw, err := root.ReadFile(timePath(key)); err == nil {
		if t, perr := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(raw))); perr == nil {
			sidecar = t
		}
	}
	// Floor at this clone's first observation: a skewed-slow writer's sidecar
	// can never make an object look older than the READER has known it.
	obs := s.observe(key)
	if sidecar.Before(obs) {
		return obs
	}
	return sidecar
}

// observe returns the first time THIS clone saw key, recording now for a key
// it has never seen. Persisted beside the clone so the floor survives
// restarts; a lost index only makes everything look newly observed (fail-safe).
func (s *fsObjectStore) observe(key string) time.Time {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	return s.observeLocked(key, time.Now().UTC())
}

func (s *fsObjectStore) observeLocked(key string, now time.Time) time.Time {
	if s.obs == nil {
		s.obs = map[string]time.Time{}
		if raw, err := os.ReadFile(s.obsPath); err == nil {
			_ = json.Unmarshal(raw, &s.obs)
		}
	}
	if t, ok := s.obs[key]; ok {
		return t
	}
	s.obs[key] = now
	s.saveObsLocked()
	return now
}

// observedAt reports the observation floor without recording a new one.
func (s *fsObjectStore) observedAt(key string) (time.Time, bool) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.obs == nil {
		s.obs = map[string]time.Time{}
		if raw, err := os.ReadFile(s.obsPath); err == nil {
			_ = json.Unmarshal(raw, &s.obs)
		}
	}
	t, ok := s.obs[key]
	return t, ok
}

func (s *fsObjectStore) forgetObservedLocked(key string) {
	if s.obs == nil {
		return
	}
	delete(s.obs, key)
	s.saveObsLocked()
}

func (s *fsObjectStore) saveObsLocked() {
	raw, err := json.Marshal(s.obs)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.obsPath), 0o700); err != nil {
		return
	}
	_ = os.WriteFile(s.obsPath, raw, 0o600)
}

func (s *fsObjectStore) PutObject(_ context.Context, key string, body []byte, ifNoneMatch bool) error {
	path, err := s.keyPath(key)
	if err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	root, err := s.openRoot()
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if ifNoneMatch {
		if _, err := root.Stat(path); err == nil {
			return ErrPreconditionFailed
		}
	}
	if err := root.MkdirAll(filepath.ToSlash(filepath.Dir(path)), 0o700); err != nil {
		return fmt.Errorf("create git hub object dir: %w", err)
	}
	if err := writeRootFileAtomic(root, path, body); err != nil {
		return fmt.Errorf("write git hub object: %w", err)
	}
	s.observeLocked(key, time.Now().UTC())
	return s.writeTimestamp(root, key)
}

func (s *fsObjectStore) GetObject(_ context.Context, key string) ([]byte, error) {
	path, err := s.keyPath(key)
	if err != nil {
		return nil, err
	}
	root, err := s.openRoot()
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", dssync.ErrBlobNotFound, key)
	}
	if err != nil {
		return nil, fmt.Errorf("read git hub object: %w", err)
	}
	return data, nil
}

func (s *fsObjectStore) ObjectExists(_ context.Context, key string) (bool, error) {
	path, err := s.keyPath(key)
	if err != nil {
		return false, err
	}
	root, err := s.openRoot()
	if err != nil {
		return false, err
	}
	defer func() { _ = root.Close() }()
	if _, err := root.Stat(path); err == nil {
		return true, nil
	} else if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else {
		return false, fmt.Errorf("stat git hub object: %w", err)
	}
}

func (s *fsObjectStore) DeleteObject(_ context.Context, key string) error {
	path, err := s.keyPath(key)
	if err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	root, err := s.openRoot()
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := root.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete git hub object: %w", err)
	}
	if err := root.Remove(timePath(key)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete git hub object sidecar: %w", err)
	}
	s.forgetObservedLocked(key)
	return nil
}

// listKeys walks the checkout collecting object keys (slash-separated,
// root-relative), skipping .git, the timestamp sidecars, and the marker.
func (s *fsObjectStore) listKeys(root *os.Root) ([]string, error) {
	var keys []string
	var staleTemps []string
	err := fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		key := path
		if d.IsDir() {
			if key == "." {
				return nil
			}
			if key == ".git" || key == strings.TrimSuffix(gitTimesPrefix, "/") || key == ".devstrap-meta" {
				return filepath.SkipDir
			}
			return nil
		}
		if key == gitMarkerFile {
			return nil
		}
		// Orphan writeFileAtomic temps (crash between create and rename):
		// never surfaced as objects, and collected for reclamation once
		// safely stale — a same-machine writer finishes in seconds and
		// another device's in-flight upload rides the cloud drive with a
		// fresh mtime, so an hour-old temp is definitionally abandoned
		// (post-review MINOR, P7-HUB-05).
		if strings.HasPrefix(filepath.Base(key), ".tmp-") {
			if info, ierr := d.Info(); ierr == nil && time.Since(info.ModTime()) > staleTempAge {
				staleTemps = append(staleTemps, path)
			}
			return nil
		}
		keys = append(keys, key)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list git hub objects: %w", err)
	}
	// Best-effort reclamation after the walk (not inside the callback — no
	// mid-walk tree mutation, and no walk-relative filesystem op for gosec's
	// G122). A failed remove just retries on the next list.
	for _, p := range staleTemps {
		_ = root.Remove(p)
	}
	sort.Strings(keys)
	return keys, nil
}

func (s *fsObjectStore) ListObjectsV2(_ context.Context, prefix, startAfter string, maxKeys int) ([]dssync.BlobInfo, string, error) {
	root, err := s.openRoot()
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = root.Close() }()
	all, err := s.listKeys(root)
	if err != nil {
		return nil, "", err
	}
	var keys []string
	for _, k := range all {
		if strings.HasPrefix(k, prefix) && k > startAfter {
			keys = append(keys, k)
		}
	}
	if maxKeys <= 0 {
		maxKeys = 1000
	}
	limit := len(keys)
	next := ""
	if len(keys) > maxKeys {
		limit = maxKeys
		next = keys[maxKeys-1]
	}
	objs := make([]dssync.BlobInfo, 0, limit)
	for _, key := range keys[:limit] {
		objs = append(objs, dssync.BlobInfo{Key: key, LastModified: s.modTime(root, key)})
	}
	return objs, next, nil
}

func (s *fsObjectStore) ListCommonPrefixes(_ context.Context, prefix, delimiter string) ([]string, error) {
	root, err := s.openRoot()
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	all, err := s.listKeys(root)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, k := range all {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		rest := strings.TrimPrefix(k, prefix)
		idx := strings.Index(rest, delimiter)
		if idx < 0 {
			continue
		}
		set[prefix+rest[:idx+len(delimiter)]] = true
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

func (s *fsObjectStore) StatObject(_ context.Context, key string) (dssync.BlobInfo, error) {
	path, err := s.keyPath(key)
	if err != nil {
		return dssync.BlobInfo{}, err
	}
	root, err := s.openRoot()
	if err != nil {
		return dssync.BlobInfo{}, err
	}
	defer func() { _ = root.Close() }()
	if _, err := root.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return dssync.BlobInfo{}, fmt.Errorf("%w: %s", dssync.ErrBlobNotFound, key)
		}
		return dssync.BlobInfo{}, fmt.Errorf("stat git hub object: %w", err)
	}
	return dssync.BlobInfo{Key: key, LastModified: s.modTime(root, key)}, nil
}

func (s *fsObjectStore) GetObjectWithETag(ctx context.Context, key string) ([]byte, string, error) {
	data, err := s.GetObject(ctx, key)
	if err != nil {
		return nil, "", err
	}
	return data, fsETag(data), nil
}

func (s *fsObjectStore) PutObjectIfMatch(_ context.Context, key string, body []byte, etag string) error {
	path, err := s.keyPath(key)
	if err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	root, err := s.openRoot()
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	current, rerr := root.ReadFile(path)
	if rerr != nil || fsETag(current) != etag {
		return ErrPreconditionFailed
	}
	if err := writeRootFileAtomic(root, path, body); err != nil {
		return fmt.Errorf("write git hub object: %w", err)
	}
	s.observeLocked(key, time.Now().UTC())
	return s.writeTimestamp(root, key)
}

func fsETag(data []byte) string {
	sum := sha256.Sum256(data)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

// clearDirExceptGit removes everything in dir except the .git metadata,
// resetting the checkout to an empty tree for the unborn-branch state.
func clearDirExceptGit(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() == ".git" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// dirEmptyExceptGit reports whether the checkout holds nothing but .git.
func dirEmptyExceptGit(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if e.Name() != ".git" {
			return false, nil
		}
	}
	return true, nil
}

// redactedRemote strips URL credentials for error text.
func redactedRemote(remote string) string {
	if i := strings.Index(remote, "://"); i >= 0 {
		rest := remote[i+3:]
		if at := strings.Index(rest, "@"); at >= 0 {
			return remote[:i+3] + "[REDACTED]@" + rest[at+1:]
		}
	}
	return remote
}
