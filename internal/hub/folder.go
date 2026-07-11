// FolderHub is the local-folder / cloud-drive-folder hub carrier (AD-1 final
// slice): a plain shared directory — a Dropbox/iCloud/Google Drive folder, an
// SMB/NFS mount, anything the OS presents as a filesystem path — carries the
// same zero-knowledge object set the R2/S3 and git-carrier backends store
// (enc.v2 event ciphertext, age blobs, sealed snapshots, signed manifests and
// acks). No bucket, no git remote, no credential plane beyond the drive the
// user already syncs.
//
// Architecture: like the git carrier, the folder carrier composes the
// ALREADY-PROVEN R2Hub semantics over the plain-filesystem fsObjectStore, but
// rooted DIRECTLY in the shared folder — there is no fetch/commit/push loop,
// because the cloud drive (or network mount) is the replication transport. Each
// dssync.Hub method is just: acquire the cross-process lock → delegate to
// R2Hub → release.
//
// Lock/observation placement: the cross-process lock file and the per-clone
// observation floor (observed.json) live in the LOCAL home cache
// (~/.devstrap/hub-folder/<hash>/), NEVER inside the shared folder. Replicating
// lock churn through a cloud drive would cause false contention and "conflicted
// copy" duplicates, and the observation floor is inherently per-device local
// state. Only the object payloads and their RFC3339Nano timestamp sidecars
// (.devstrap-meta/times/, the same freshness mechanism the git carrier uses)
// live in the shared folder, where they must replicate to converge.
//
// CAS is best-effort across devices. The fsObjectStore conditional-put
// (PutObjectIfMatch / If-None-Match) is atomic only under the cross-process
// lock, and that lock lives in each device's LOCAL cache — so it serializes
// multiple processes of the SAME device but NOT two different devices writing
// through the same drive. A cloud drive gives no cross-writer linearization
// point (unlike the git carrier's atomic push-ref CAS or R2's conditional PUT),
// so a simultaneous retention/sweep-lock race between two devices can in
// principle produce two "winners"; the drive resolves it as a conflicted copy.
// This is the documented residual (spec/15), in the same advisory-cooperation
// class as the sweep lock's byzantine residuals — acceptable because the folder
// carrier targets the single-user, few-devices, rarely-simultaneous case, and
// every object is content-addressed or (device,seq)-unique so ordinary
// convergence never collides.
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
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/Reederey87/DevStrap/internal/platform"
	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
)

// FolderHub implements dssync.Hub over a shared directory. Construct with
// NewFolderHub. Safe for concurrent use: every operation serializes on an
// in-process mutex plus a cross-process lock file (both keyed to this device's
// local cache, so they serialize same-device processes; cross-device ordering
// is left to the drive — see the package comment).
type FolderHub struct {
	workspaceID string
	dir         string // the shared folder (the object store root)
	lockPath    string // cross-process lock, in the LOCAL cache (never in dir)
	mu          sync.Mutex
	store       *fsObjectStore
	r2          R2Hub
	// sleep / lockWait / lockHeartbeat are test seams for the lock timing,
	// mirroring GitCarrierHub.
	sleep         func(time.Duration)
	lockWait      time.Duration
	lockHeartbeat time.Duration
}

// Compile-time assertion that *FolderHub satisfies dssync.Hub.
var _ dssync.Hub = (*FolderHub)(nil)

// NewFolderHub prepares a folder-carrier hub rooted at dir (which must be an
// absolute path). cacheRoot is the local cache PARENT (one subdirectory per
// resolved folder, so two folders never share a lock or observation floor); the
// lock file and observed.json live under it, never inside the shared folder.
// The folder is created (0700) when missing and its symlinks are resolved once
// — cloud-drive roots are frequently symlinks — so the store, the lock, and the
// cache hash all key on the real path.
func NewFolderHub(dir, workspaceID, cacheRoot string) (*FolderHub, error) {
	if workspaceID == "" {
		return nil, errors.New("folder hub: empty workspace id")
	}
	if dir == "" {
		return nil, errors.New("folder hub: empty folder path")
	}
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("folder hub: path %q must be absolute", dir)
	}
	if cacheRoot == "" {
		return nil, errors.New("folder hub: empty cache root")
	}
	if info, err := os.Stat(dir); err == nil {
		if !info.IsDir() {
			return nil, fmt.Errorf("folder hub: path %q is not a directory", dir)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("folder hub: stat %q: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("folder hub: create folder %q: %w", dir, err)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, fmt.Errorf("folder hub: resolve folder %q: %w", dir, err)
	}
	rootInfo, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("folder hub: stat resolved folder %q: %w", resolved, err)
	}
	sum := sha256.Sum256([]byte(resolved))
	base := filepath.Join(cacheRoot, hex.EncodeToString(sum[:])[:16])
	store := &fsObjectStore{root: resolved, rootInfo: rootInfo, obsPath: filepath.Join(base, "observed.json")}
	return &FolderHub{
		workspaceID:   workspaceID,
		dir:           resolved,
		lockPath:      filepath.Join(base, "folder.lock"),
		store:         store,
		r2:            R2Hub{S3: store, WorkspaceID: workspaceID},
		sleep:         time.Sleep,
		lockWait:      fsLockWait,
		lockHeartbeat: fsLockHeartbeat,
	}, nil
}

// lock takes the shared cross-process file lock, mirroring GitCarrierHub.lock.
func (f *FolderHub) lock() (func(), error) {
	return fsLock{
		mu:        &f.mu,
		path:      f.lockPath,
		wait:      f.lockWait,
		heartbeat: f.lockHeartbeat,
		stale:     fsLockStale,
		sleep:     f.sleep,
	}.acquire()
}

// guard runs fn under the cross-process lock. Because the shared drive is the
// replication transport (no fetch/reset before, no commit/push after), a folder
// operation is exactly lock → revalidate root → delegate → unlock.
func (f *FolderHub) guard(fn func() error) error {
	release, err := f.lock()
	if err != nil {
		return err
	}
	defer release()
	if err := f.revalidateRoot(); err != nil {
		return err
	}
	return fn()
}

// revalidateRoot re-resolves the shared folder at use time and refuses to
// operate when it no longer denotes the directory registered at construction.
// The constructor resolves symlinks once, but the folder carrier's root is a
// replicated/shared directory — unlike the git carrier's private clone — so a
// root (or any parent component) later swapped for a symlink would otherwise
// redirect every read and write outside the registered folder. After this
// check, each object-store call opens an os.Root and verifies that handle still
// denotes the directory registered here; all shared-tree file APIs then ride
// that handle until the call completes. Same use-time revalidation stance as
// the scan symlink boundary.
func (f *FolderHub) revalidateRoot() error {
	real, err := filepath.EvalSymlinks(f.dir)
	if err != nil {
		return fmt.Errorf("folder hub: revalidate root %q: %w", f.dir, err)
	}
	if real != f.dir {
		return fmt.Errorf("folder hub: root %q now resolves to %q; refusing to operate outside the registered folder", f.dir, real)
	}
	return nil
}

// --- dssync.Hub: event plane ---

func (f *FolderHub) Push(ctx context.Context, events []state.Event) error {
	return f.guard(func() error { return f.r2.Push(ctx, events) })
}

func (f *FolderHub) Pull(ctx context.Context, after dssync.Cursor) ([]state.Event, error) {
	var events []state.Event
	err := f.guard(func() (ferr error) {
		events, ferr = f.r2.Pull(ctx, after)
		return ferr
	})
	return events, err
}

func (f *FolderHub) DeleteDeviceStream(ctx context.Context, deviceID string) (int, error) {
	var deleted int
	err := f.guard(func() (ferr error) {
		deleted, ferr = f.r2.DeleteDeviceStream(ctx, deviceID)
		return ferr
	})
	return deleted, err
}

// MigrateLegacyEvents delegates for symmetric reporting; the folder carrier,
// like the git carrier, never had the retired HLC-keyed layout, so it is a
// structural no-op. Both the dry run and the real run only read/rewrite object
// files, so a single lock covers either.
func (f *FolderHub) MigrateLegacyEvents(ctx context.Context, dryRun bool) (int, int, error) {
	var migrated, kept int
	err := f.guard(func() (ferr error) {
		migrated, kept, ferr = f.r2.MigrateLegacyEvents(ctx, dryRun)
		return ferr
	})
	return migrated, kept, err
}

// HasEvents reports whether any event was ever recorded on this hub (the
// doctor --remote capability probe, mirroring GitCarrierHub.HasEvents).
func (f *FolderHub) HasEvents(ctx context.Context) (bool, error) {
	var has bool
	err := f.guard(func() error {
		objs, _, lerr := f.store.ListObjectsV2(ctx, fmt.Sprintf("workspaces/%s/eventlog/", f.workspaceID), "", 1)
		if lerr != nil {
			return lerr
		}
		has = len(objs) > 0
		return nil
	})
	return has, err
}

// --- dssync.Hub: blob plane ---

func (f *FolderHub) PutBlob(ctx context.Context, sha256Hex string, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("read blob: %w", err)
	}
	return f.guard(func() error { return f.r2.PutBlob(ctx, sha256Hex, bytes.NewReader(data)) })
}

func (f *FolderHub) GetBlob(ctx context.Context, sha256Hex string) (io.ReadCloser, error) {
	var rc io.ReadCloser
	err := f.guard(func() (ferr error) {
		rc, ferr = f.r2.GetBlob(ctx, sha256Hex)
		return ferr
	})
	return rc, err
}

func (f *FolderHub) DeleteBlob(ctx context.Context, sha256Hex string) error {
	return f.guard(func() error { return f.r2.DeleteBlob(ctx, sha256Hex) })
}

func (f *FolderHub) ListBlobs(ctx context.Context) ([]dssync.BlobInfo, error) {
	var infos []dssync.BlobInfo
	err := f.guard(func() (ferr error) {
		infos, ferr = f.r2.ListBlobs(ctx)
		return ferr
	})
	return infos, err
}

func (f *FolderHub) StatBlob(ctx context.Context, sha256Hex string) (dssync.BlobInfo, error) {
	var info dssync.BlobInfo
	err := f.guard(func() (ferr error) {
		info, ferr = f.r2.StatBlob(ctx, sha256Hex)
		return ferr
	})
	return info, err
}

// --- dssync.Hub: retention manifest (CAS head object) ---

func (f *FolderHub) GetRetention(ctx context.Context) ([]byte, string, error) {
	var (
		raw  []byte
		etag string
	)
	err := f.guard(func() (ferr error) {
		raw, etag, ferr = f.r2.GetRetention(ctx)
		return ferr
	})
	return raw, etag, err
}

func (f *FolderHub) PutRetention(ctx context.Context, raw []byte, ifMatchETag string) error {
	return f.guard(func() error { return f.r2.PutRetention(ctx, raw, ifMatchETag) })
}

// --- dssync.Hub: sealed snapshot objects ---

func (f *FolderHub) PutSnapshotObject(ctx context.Context, sha256Hex string, body []byte) error {
	return f.guard(func() error { return f.r2.PutSnapshotObject(ctx, sha256Hex, body) })
}

func (f *FolderHub) GetSnapshotObject(ctx context.Context, sha256Hex string) ([]byte, error) {
	var body []byte
	err := f.guard(func() (ferr error) {
		body, ferr = f.r2.GetSnapshotObject(ctx, sha256Hex)
		return ferr
	})
	return body, err
}

func (f *FolderHub) ListSnapshotObjects(ctx context.Context) ([]dssync.BlobInfo, error) {
	var infos []dssync.BlobInfo
	err := f.guard(func() (ferr error) {
		infos, ferr = f.r2.ListSnapshotObjects(ctx)
		return ferr
	})
	return infos, err
}

func (f *FolderHub) DeleteSnapshotObject(ctx context.Context, sha256Hex string) error {
	return f.guard(func() error { return f.r2.DeleteSnapshotObject(ctx, sha256Hex) })
}

// --- dssync.Hub: signed per-device sync acks ---

func (f *FolderHub) PutAck(ctx context.Context, deviceID string, raw []byte) error {
	return f.guard(func() error { return f.r2.PutAck(ctx, deviceID, raw) })
}

func (f *FolderHub) ListAcks(ctx context.Context) (map[string][]byte, error) {
	var acks map[string][]byte
	err := f.guard(func() (ferr error) {
		acks, ferr = f.r2.ListAcks(ctx)
		return ferr
	})
	return acks, err
}

func (f *FolderHub) DeleteAck(ctx context.Context, deviceID string) error {
	return f.guard(func() error { return f.r2.DeleteAck(ctx, deviceID) })
}

// --- dssync.Hub: advisory sweep lock ---

func (f *FolderHub) GetSweepLock(ctx context.Context) ([]byte, time.Time, error) {
	var (
		raw []byte
		mod time.Time
	)
	err := f.guard(func() (ferr error) {
		raw, mod, ferr = f.r2.GetSweepLock(ctx)
		return ferr
	})
	// Clamp the lock's age DOWN to this clone's observation floor, exactly as
	// GitCarrierHub does: a future-dated sidecar (a skewed or hostile holder
	// clock) must not make a dead holder's lock unbreakable — once THIS reader
	// has watched the lock for a full TTL it is stale regardless of its
	// self-reported time.
	if err == nil {
		if obs, ok := f.store.observedAt(fmt.Sprintf("workspaces/%s/meta/sweep.lock", f.workspaceID)); ok && obs.Before(mod) {
			mod = obs
		}
	}
	return raw, mod, err
}

func (f *FolderHub) PutSweepLock(ctx context.Context, raw []byte) error {
	return f.guard(func() error { return f.r2.PutSweepLock(ctx, raw) })
}

func (f *FolderHub) DeleteSweepLock(ctx context.Context) error {
	return f.guard(func() error { return f.r2.DeleteSweepLock(ctx) })
}

// --- dssync.Hub: event-log compaction ---

// CompactEventsBelow deletes cold event objects. Unlike the git carrier there
// is no history to squash — file deletion IS the reclamation on a plain
// filesystem — so it is a single locked delegation to R2Hub. The caller holds
// the advisory sweep lock.
func (f *FolderHub) CompactEventsBelow(ctx context.Context, floors dssync.Cursor) (int, error) {
	var deleted int
	err := f.guard(func() (ferr error) {
		deleted, ferr = f.r2.CompactEventsBelow(ctx, floors)
		return ferr
	})
	return deleted, err
}

// --- shared cross-process filesystem lock ---

// fsLock is the cross-process file lock shared by the filesystem-backed hub
// carriers (git and folder). It pairs an in-process mutex with an O_EXCL lock
// file whose mtime a heartbeat goroutine keeps warm while held. Same-host
// owner liveness is authoritative: a live holder is never broken regardless
// of mtime, with the PID paired to an opaque platform start-time identity so
// a recycled PID reads as dead rather than wedging the lock forever
// (mirroring the repo-lock semantics from P7-GIT-03). Legacy, corrupt, and
// cross-host records use the stale TTL. The lock file lives OUTSIDE any
// synced tree.
type fsLockOwner struct {
	Version    int    `json:"version"`
	PID        int    `json:"pid"`
	Hostname   string `json:"hostname"`
	Nonce      string `json:"nonce"`
	AcquiredAt string `json:"acquired_at"`
	// StartedAt is the opaque same-host process start identity from
	// platform.ProcessStartTime; 0 when the platform cannot supply one.
	StartedAt int64 `json:"started_at,omitempty"`
}

type fsLock struct {
	mu        *sync.Mutex
	path      string
	wait      time.Duration
	heartbeat time.Duration
	stale     time.Duration
	sleep     func(time.Duration)
}

var hubProcessAlive = func(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return true // liveness is indeterminate, not provably dead
	}
	return !errors.Is(process.Signal(syscall.Signal(0)), syscall.ESRCH)
}

// hubProcessStartTime is a test seam over the platform start-time identity.
var hubProcessStartTime = platform.ProcessStartTime

// hubLockOwnerAlive reports whether the recorded same-host owner is still the
// process that took the lock. A live PID with a different start identity is a
// recycled PID — the real holder is dead (P7-GIT-03 semantics). A zero or
// unavailable identity keeps the conservative liveness-only answer. A false
// "alive" (Linux boot-relative tick collision after reboot, or identity 0)
// deliberately has no mtime backstop — the timeout error names the owner for
// manual recovery — because any TTL override would reintroduce the suspended-
// holder steal this design exists to prevent (spec/15 residual).
func hubLockOwnerAlive(owner fsLockOwner) bool {
	if !hubProcessAlive(owner.PID) {
		return false
	}
	if owner.StartedAt == 0 {
		return true
	}
	current, err := hubProcessStartTime(owner.PID)
	if err != nil {
		return true // identity indeterminate: never steal on a lookup failure
	}
	return current == owner.StartedAt
}

// stageOwnerRecord writes this process's complete owner record to a private
// temp file beside the lock, ready for an atomic link-publish. The caller
// removes the temp file whether or not the link wins.
func (l fsLock) stageOwnerRecord() (fsLockOwner, string, error) {
	nonceBytes := make([]byte, 16)
	if _, err := rand.Read(nonceBytes); err != nil {
		return fsLockOwner{}, "", fmt.Errorf("generate hub lock nonce: %w", err)
	}
	hostname, err := os.Hostname()
	if err != nil {
		return fsLockOwner{}, "", fmt.Errorf("get hostname for hub lock: %w", err)
	}
	startedAt, _ := hubProcessStartTime(os.Getpid()) // 0 on unsupported platforms: liveness-only fallback
	owner := fsLockOwner{
		Version:    1,
		PID:        os.Getpid(),
		Hostname:   hostname,
		Nonce:      hex.EncodeToString(nonceBytes),
		AcquiredAt: time.Now().Format(time.RFC3339),
		StartedAt:  startedAt,
	}
	raw, err := json.Marshal(owner)
	if err != nil {
		return fsLockOwner{}, "", fmt.Errorf("marshal hub lock owner: %w", err)
	}
	tmpName := l.path + ".stage-" + owner.Nonce
	if err := os.WriteFile(tmpName, raw, 0o600); err != nil { //nolint:gosec // private hub cache dir
		return fsLockOwner{}, "", fmt.Errorf("stage hub lock owner: %w", err)
	}
	return owner, tmpName, nil
}

// acquire takes the in-process mutex plus the cross-process lock file and
// returns the release func.
func (l fsLock) acquire() (func(), error) {
	l.mu.Lock()
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		l.mu.Unlock()
		return nil, fmt.Errorf("create hub cache dir: %w", err)
	}
	deadline := time.Now().Add(l.wait)
	for {
		// Publish atomically: build the COMPLETE owner record in a private
		// temp file, then os.Link it to the lock path. Link fails with EEXIST
		// when the lock is held (the same mutual exclusion O_EXCL gave), and a
		// contender can never observe an empty or torn record — the previous
		// create-then-write shape had a window where a suspension between the
		// two left a fresh empty file that aged into a TTL break, stealing a
		// live holder's lock (CodeRabbit).
		owner, tmpName, err := l.stageOwnerRecord()
		if err != nil {
			l.mu.Unlock()
			return nil, err
		}
		err = os.Link(tmpName, l.path)
		_ = os.Remove(tmpName)
		if err == nil {
			stop := make(chan struct{})
			done := make(chan struct{})
			go func() {
				defer close(done)
				ticker := time.NewTicker(l.heartbeat)
				defer ticker.Stop()
				for {
					select {
					case <-stop:
						return
					case <-ticker.C:
						now := time.Now()
						if err := os.Chtimes(l.path, now, now); errors.Is(err, os.ErrNotExist) {
							return
						}
					}
				}
			}()
			return func() {
				close(stop)
				<-done
				raw, readErr := os.ReadFile(l.path) //nolint:gosec // lock path is the private hub cache lock
				var current fsLockOwner
				if readErr == nil && json.Unmarshal(raw, &current) == nil && current.Nonce == owner.Nonce {
					_ = os.Remove(l.path)
				}
				l.mu.Unlock()
			}, nil
		}
		if !os.IsExist(err) {
			l.mu.Unlock()
			return nil, fmt.Errorf("acquire hub lock %s: %w", l.path, err)
		}
		before, readErr := os.ReadFile(l.path) //nolint:gosec // lock path is the private hub cache lock
		beforeInfo, beforeStatErr := os.Stat(l.path)
		if os.IsNotExist(readErr) || os.IsNotExist(beforeStatErr) {
			continue
		}
		if l.isStale() {
			after, rereadErr := os.ReadFile(l.path) //nolint:gosec // lock path is the private hub cache lock
			afterInfo, afterStatErr := os.Stat(l.path)
			unchanged := readErr == nil && rereadErr == nil && bytes.Equal(before, after)
			if readErr != nil && rereadErr != nil && beforeStatErr == nil && afterStatErr == nil {
				// An unreadable legacy/corrupt lock must still age out. With no
				// bytes to compare, require stable file identity and metadata.
				unchanged = os.SameFile(beforeInfo, afterInfo) &&
					beforeInfo.Size() == afterInfo.Size() &&
					beforeInfo.ModTime().Equal(afterInfo.ModTime())
			}
			if unchanged {
				if removeErr := os.Remove(l.path); removeErr == nil || os.IsNotExist(removeErr) {
					continue
				}
			}
		}
		if time.Now().After(deadline) {
			if owner, ok := readFSLockOwner(l.path); ok {
				l.mu.Unlock()
				return nil, fmt.Errorf("acquire hub lock %s: timed out; held by pid %d on %s since %s", l.path, owner.PID, owner.Hostname, owner.AcquiredAt)
			}
			l.mu.Unlock()
			return nil, fmt.Errorf("acquire hub lock %s: timed out (another devstrap process is using this hub?)", l.path)
		}
		l.sleep(50 * time.Millisecond)
	}
}

func (l fsLock) isStale() bool {
	raw, err := os.ReadFile(l.path) //nolint:gosec // lock path is the private hub cache lock
	var owner fsLockOwner
	if err == nil && len(raw) > 0 && json.Unmarshal(raw, &owner) == nil && validFSLockOwner(owner) {
		hostname, hostErr := os.Hostname()
		if hostErr == nil && owner.Hostname == hostname {
			return !hubLockOwnerAlive(owner)
		}
	}
	info, statErr := os.Stat(l.path)
	return statErr == nil && time.Since(info.ModTime()) > l.stale
}

// validFSLockOwner gates the owner-aware staleness rules on a COMPLETE record:
// json.Unmarshal accepts partial/unknown-shape JSON, and a fragment like
// {"hostname":"<local>"} would otherwise read PID 0 as a provably dead owner
// and break the lock instantly. Anything less than a full v1 record is judged
// by the mtime TTL like every other unparseable lock (CodeRabbit).
func validFSLockOwner(o fsLockOwner) bool {
	return o.Version == 1 && o.PID > 0 && o.Hostname != "" && o.Nonce != ""
}

func readFSLockOwner(path string) (fsLockOwner, bool) {
	raw, err := os.ReadFile(path) //nolint:gosec // path is the private hub cache lock
	if err != nil || len(raw) == 0 {
		return fsLockOwner{}, false
	}
	var owner fsLockOwner
	if json.Unmarshal(raw, &owner) != nil {
		return fsLockOwner{}, false
	}
	return owner, true
}
