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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
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
	// gitLockStale is when a same-machine cross-process lock is considered
	// abandoned. Carrier operations can legitimately hold the lock for a long
	// fetch, so this is far laxer than FileHub's 10s.
	gitLockStale = 30 * time.Minute
	// gitLockWait is how long a second local process waits for the lock.
	gitLockWait = 2 * time.Minute
)

// gitCarrierMarker is the content of gitMarkerFile.
type gitCarrierMarker struct {
	Version     int    `json:"version"`
	WorkspaceID string `json:"workspace_id"`
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
	store := &fsObjectStore{root: dir}
	return &GitCarrierHub{
		remote:      remote,
		branch:      branch,
		workspaceID: workspaceID,
		dir:         dir,
		lockPath:    filepath.Join(base, "repo.lock"),
		runner:      git.NewRunner(),
		store:       store,
		r2:          R2Hub{S3: store, WorkspaceID: workspaceID},
		sleep:       time.Sleep,
	}, nil
}

// lock takes the in-process mutex plus the cross-process lock file and returns
// the release func. The lock file lives OUTSIDE the checkout so git clean can
// never remove a held lock.
func (g *GitCarrierHub) lock() (func(), error) {
	g.mu.Lock()
	if err := os.MkdirAll(filepath.Dir(g.lockPath), 0o700); err != nil {
		g.mu.Unlock()
		return nil, fmt.Errorf("create git hub cache dir: %w", err)
	}
	deadline := time.Now().Add(gitLockWait)
	for {
		f, err := os.OpenFile(g.lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // lock file under the private hub cache dir
		if err == nil {
			_ = f.Close()
			return func() {
				_ = os.Remove(g.lockPath)
				g.mu.Unlock()
			}, nil
		}
		if !os.IsExist(err) {
			g.mu.Unlock()
			return nil, fmt.Errorf("acquire git hub lock %s: %w", g.lockPath, err)
		}
		if info, serr := os.Stat(g.lockPath); serr == nil && time.Since(info.ModTime()) > gitLockStale {
			_ = os.Remove(g.lockPath) // abandoned by a crashed process
			continue
		}
		if time.Now().After(deadline) {
			g.mu.Unlock()
			return nil, fmt.Errorf("acquire git hub lock %s: timed out (another devstrap process is using this hub?)", g.lockPath)
		}
		g.sleep(50 * time.Millisecond)
	}
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
	_, err := g.runner.Run(ctx, g.dir, "fetch", "--quiet", "origin", "+refs/heads/"+g.branch)
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
		g.fetchedSHA = sha
		return g.validateMarkerLocked()
	case errors.Is(err, git.ErrBranchNotFound):
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
// to; a marker for a different workspace is refused rather than mixed.
func (g *GitCarrierHub) validateMarkerLocked() error {
	raw, err := os.ReadFile(filepath.Join(g.dir, gitMarkerFile))
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

// writeMarkerLocked seeds the carrier marker on a bootstrap write.
func (g *GitCarrierHub) writeMarkerLocked() error {
	path := filepath.Join(g.dir, gitMarkerFile)
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	raw, err := json.Marshal(gitCarrierMarker{Version: 1, WorkspaceID: g.workspaceID})
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
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
		_, err = g.runner.Run(ctx, g.dir, "push", "--quiet", "origin", "HEAD:refs/heads/"+g.branch)
		if err == nil {
			return nil
		}
		if !errors.Is(err, git.ErrNonFastForward) {
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
// retired HLC-keyed layout. Delegated for symmetric reporting.
func (g *GitCarrierHub) MigrateLegacyEvents(ctx context.Context, dryRun bool) (int, int, error) {
	var migrated, kept int
	err := g.writeLoop(ctx, "migrate-events", func() (ferr error) {
		migrated, kept, ferr = g.r2.MigrateLegacyEvents(ctx, dryRun)
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
			return deleted, nil
		}
		if !errors.Is(err, git.ErrNonFastForward) {
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
type fsObjectStore struct {
	root string
	// wmu guards concurrent writers within one process (R2Hub's push/pull
	// fan-out calls S3Client methods concurrently; distinct keys write
	// distinct files, but MkdirAll+WriteFile pairs stay simplest serialized).
	wmu sync.Mutex
}

func (s *fsObjectStore) keyPath(key string) (string, error) {
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "\\") || strings.Contains(key, "..") {
		return "", fmt.Errorf("%w: %q", dssync.ErrInvalidBlobKey, key)
	}
	return filepath.Join(s.root, filepath.FromSlash(key)), nil
}

func (s *fsObjectStore) timePath(key string) string {
	return filepath.Join(s.root, filepath.FromSlash(gitTimesPrefix+key))
}

func (s *fsObjectStore) writeTimestamp(key string) error {
	path := s.timePath(key)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(time.Now().UTC().Format(time.RFC3339Nano)), 0o600)
}

func (s *fsObjectStore) modTime(key string) time.Time {
	raw, err := os.ReadFile(s.timePath(key))
	if err != nil {
		return time.Time{} // no sidecar: zero time reads as "keep" to gc (fail-safe)
	}
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(raw)))
	if err != nil {
		return time.Time{}
	}
	return t
}

func (s *fsObjectStore) PutObject(_ context.Context, key string, body []byte, ifNoneMatch bool) error {
	path, err := s.keyPath(key)
	if err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if ifNoneMatch {
		if _, err := os.Stat(path); err == nil {
			return ErrPreconditionFailed
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create git hub object dir: %w", err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return fmt.Errorf("write git hub object: %w", err)
	}
	return s.writeTimestamp(key)
}

func (s *fsObjectStore) GetObject(_ context.Context, key string) ([]byte, error) {
	path, err := s.keyPath(key)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path) //nolint:gosec // keyPath confines the key under the carrier checkout root
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
	if _, err := os.Stat(path); err == nil {
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
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete git hub object: %w", err)
	}
	if err := os.Remove(s.timePath(key)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete git hub object sidecar: %w", err)
	}
	return nil
}

// listKeys walks the checkout collecting object keys (slash-separated,
// root-relative), skipping .git, the timestamp sidecars, and the marker.
func (s *fsObjectStore) listKeys() ([]string, error) {
	var keys []string
	err := filepath.WalkDir(s.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		rel, rerr := filepath.Rel(s.root, path)
		if rerr != nil {
			return rerr
		}
		key := filepath.ToSlash(rel)
		if d.IsDir() {
			if key == ".git" || key == strings.TrimSuffix(gitTimesPrefix, "/") || key == ".devstrap-meta" {
				return filepath.SkipDir
			}
			return nil
		}
		if key == gitMarkerFile {
			return nil
		}
		keys = append(keys, key)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list git hub objects: %w", err)
	}
	sort.Strings(keys)
	return keys, nil
}

func (s *fsObjectStore) ListObjectsV2(_ context.Context, prefix, startAfter string, maxKeys int) ([]dssync.BlobInfo, string, error) {
	all, err := s.listKeys()
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
		objs = append(objs, dssync.BlobInfo{Key: key, LastModified: s.modTime(key)})
	}
	return objs, next, nil
}

func (s *fsObjectStore) ListCommonPrefixes(_ context.Context, prefix, delimiter string) ([]string, error) {
	all, err := s.listKeys()
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
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return dssync.BlobInfo{}, fmt.Errorf("%w: %s", dssync.ErrBlobNotFound, key)
		}
		return dssync.BlobInfo{}, fmt.Errorf("stat git hub object: %w", err)
	}
	return dssync.BlobInfo{Key: key, LastModified: s.modTime(key)}, nil
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
	current, rerr := os.ReadFile(path) //nolint:gosec // keyPath confines the key under the carrier checkout root
	if rerr != nil || fsETag(current) != etag {
		return ErrPreconditionFailed
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return fmt.Errorf("write git hub object: %w", err)
	}
	return s.writeTimestamp(key)
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
