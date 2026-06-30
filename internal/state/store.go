package state

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/id"
	"github.com/Reederey87/DevStrap/internal/pathkey"
	"github.com/Reederey87/DevStrap/internal/platform"
	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

type Store struct {
	db          *sql.DB
	keyDir      string
	workspaceMu sync.RWMutex
	workspaceID string
}

var ErrNotInitialized = errors.New("workspace is not initialized; run devstrap init")
var ErrDivergentEvent = errors.New("event id already exists with different immutable content")
var ErrEventHashChain = errors.New("event prev_event_hash chain break")

const (
	hlcLogicalBits  = 16
	hlcLogicalMask  = (1 << hlcLogicalBits) - 1
	timestampLayout = "2006-01-02T15:04:05.000000000Z"
)

type Summary struct {
	WorkspaceName string          `json:"workspace_name"`
	RootPath      string          `json:"root_path"`
	ProjectCount  int             `json:"project_count"`
	DeviceID      string          `json:"device_id,omitempty"`
	Projects      []ProjectStatus `json:"projects,omitempty"`
}

type Device struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	OS               string `json:"os"`
	Arch             string `json:"arch"`
	Hostname         string `json:"hostname,omitempty"`
	PublicKey        string `json:"public_key,omitempty"`
	SigningPublicKey string `json:"signing_public_key,omitempty"`
	TrustState       string `json:"trust_state"`
}

type NamespaceEntry struct {
	ID                    string `json:"id"`
	Path                  string `json:"path"`
	PathKey               string `json:"path_key"`
	Type                  string `json:"type"`
	DisplayName           string `json:"display_name,omitempty"`
	MaterializationPolicy string `json:"materialization_policy"`
	Status                string `json:"status"`
	SourceEventHLC        int64  `json:"source_event_hlc,omitempty"`
	SourceEventDeviceID   string `json:"source_event_device_id,omitempty"`
	SourceEventID         string `json:"source_event_id,omitempty"`
}

type GitRepo struct {
	NamespaceID   string `json:"namespace_id"`
	RemoteURL     string `json:"remote_url"`
	RemoteKey     string `json:"remote_key"`
	DefaultBranch string `json:"default_branch"`
	CloneFilter   string `json:"clone_filter,omitempty"`
	LFSPolicy     string `json:"lfs_policy"`
	ForgeKind     string `json:"forge_kind,omitempty"`
}

type EnvProfile struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	Name        string `json:"name"`
	Provider    string `json:"provider"`
	Mode        string `json:"mode"`
}

type SecretBinding struct {
	ID                string `json:"id"`
	EnvProfileID      string `json:"env_profile_id"`
	VarName           string `json:"var_name"`
	ProviderRef       string `json:"provider_ref,omitempty"`
	EncryptedValueRef string `json:"encrypted_value_ref,omitempty"`
	Required          bool   `json:"required"`
	NeedsRotation     bool   `json:"needs_rotation,omitempty"`
}

type ProjectStatus struct {
	NamespaceEntry
	RemoteURL            string `json:"remote_url,omitempty"`
	RemoteKey            string `json:"remote_key,omitempty"`
	DefaultBranch        string `json:"default_branch,omitempty"`
	LFSPolicy            string `json:"lfs_policy,omitempty"`
	ForgeKind            string `json:"forge_kind,omitempty"`
	LocalPath            string `json:"local_path,omitempty"`
	MaterializationState string `json:"materialization_state,omitempty"`
	DirtyState           string `json:"dirty_state,omitempty"`
}

func timestampNow() string {
	return formatTimestamp(time.Now())
}

func formatTimestamp(t time.Time) string {
	return t.UTC().Format(timestampLayout)
}

type UpsertProjectParams struct {
	Path                  string
	Type                  string
	RemoteURL             string
	RemoteKey             string
	DefaultBranch         string
	LFSPolicy             string
	MaterializationPolicy string
	LocalPath             string
	MaterializationState  string
	DirtyState            string
	SourceEventHLC        int64
	SourceEventDeviceID   string
	SourceEventID         string
	ForgeKind             string
}

type Conflict struct {
	ID          string `json:"id"`
	NamespaceID string `json:"namespace_id,omitempty"`
	Type        string `json:"type"`
	Status      string `json:"status"`
	DetailsJSON string `json:"details_json"`
}

type Event struct {
	ID            string
	WorkspaceID   string
	DeviceID      string
	Seq           int64
	HLC           int64
	Type          string
	PayloadJSON   string
	ContentHash   string
	DeviceSig     string
	PrevEventHash string
	CreatedAt     string
}

type Tx struct {
	tx          *sql.Tx
	workspaceID string
}

type sqlExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type Worktree struct {
	ID          string `json:"id"`
	NamespaceID string `json:"namespace_id"`
	DeviceID    string `json:"device_id"`
	Path        string `json:"path"`
	Branch      string `json:"branch"`
	BaseRef     string `json:"base_ref"`
	BaseSHA     string `json:"base_sha"`
	CreatedBy   string `json:"created_by"`
	Status      string `json:"status"`
	DirtyState  string `json:"dirty_state"`
}

type AgentRun struct {
	ID          string `json:"id"`
	NamespaceID string `json:"namespace_id"`
	WorktreeID  string `json:"worktree_id,omitempty"`
	Engine      string `json:"engine"`
	Task        string `json:"task"`
	PolicyID    string `json:"policy_id,omitempty"`
	Status      string `json:"status"`
	BaseRef     string `json:"base_ref,omitempty"`
	BaseSHA     string `json:"base_sha,omitempty"`
	Branch      string `json:"branch,omitempty"`
	LogPath     string `json:"log_path,omitempty"`
	DiffSummary string `json:"diff_summary,omitempty"`
	TestSummary string `json:"test_summary,omitempty"`
}

func Open(ctx context.Context, path string) (*Store, error) {
	dsn := sqliteDSN(path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite state: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	// CODE-05: use PingContext so a Ctrl-C during a slow/locked open can be
	// cancelled instead of blocking on context.Background().
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("configure sqlite state: %w", err)
	}
	if err := assertForeignKeysEnabled(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := foreignKeyCheck(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = db.Close()
		return nil, fmt.Errorf("secure sqlite state: %w", err)
	}
	return &Store{db: db, keyDir: filepath.Join(filepath.Dir(path), "keys")}, nil
}

func sqliteDSN(path string) string {
	q := url.Values{}
	for _, pragma := range []string{
		"busy_timeout(5000)",
		"foreign_keys(1)",
		"journal_mode(WAL)",
		"synchronous(NORMAL)",
		"journal_size_limit(67108864)",
	} {
		q.Add("_pragma", pragma)
	}
	q.Set("_txlock", "immediate")
	return (&url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}).String()
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Migrate() error {
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		return fmt.Errorf("set migration dialect: %w", err)
	}
	if err := goose.Up(s.db, "migrations"); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}
	return nil
}

func (s *Store) Version() (int64, error) {
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		return 0, fmt.Errorf("set migration dialect: %w", err)
	}
	version, err := goose.GetDBVersion(s.db)
	if err != nil {
		return 0, fmt.Errorf("read migration version: %w", err)
	}
	return version, nil
}

func (s *Store) Down() error {
	goose.SetBaseFS(migrationsFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		return fmt.Errorf("set migration dialect: %w", err)
	}
	if err := goose.Down(s.db, "migrations"); err != nil {
		return fmt.Errorf("roll back migration: %w", err)
	}
	return nil
}

func (s *Store) Backup(ctx context.Context, outputPath string) error {
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", outputPath); err != nil {
		return fmt.Errorf("backup state database: %w", err)
	}
	if err := os.Chmod(outputPath, 0o600); err != nil {
		return fmt.Errorf("secure state backup: %w", err)
	}
	// DATA-01: validate the backup after VACUUM INTO so corruption is caught
	// before a restore depends on it. Remove the partial backup on failure.
	if err := validateBackup(ctx, outputPath); err != nil {
		_ = os.Remove(outputPath)
		return fmt.Errorf("backup failed validation: %w", err)
	}
	return nil
}

func validateBackup(ctx context.Context, path string) error {
	dsn := sqliteDSN(path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("open backup for validation: %w", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping backup: %w", err)
	}
	var quickResult string
	if err := db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&quickResult); err != nil {
		return fmt.Errorf("run quick_check on backup: %w", err)
	}
	if quickResult != "ok" {
		return fmt.Errorf("backup quick_check failed: %s", quickResult)
	}
	return foreignKeyCheck(ctx, db)
}

func (s *Store) QuickCheck(ctx context.Context) (string, error) {
	var result string
	if err := s.db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&result); err != nil {
		return "", fmt.Errorf("run sqlite quick_check: %w", err)
	}
	return result, nil
}

func (s *Store) ForeignKeyCheck(ctx context.Context) (string, error) {
	if err := foreignKeyCheck(ctx, s.db); err != nil {
		return "", err
	}
	return "ok", nil
}

func assertForeignKeysEnabled(db *sql.DB) error {
	var enabled int
	if err := db.QueryRow("PRAGMA foreign_keys").Scan(&enabled); err != nil {
		return fmt.Errorf("read sqlite foreign_keys pragma: %w", err)
	}
	if enabled != 1 {
		return fmt.Errorf("sqlite foreign key enforcement is disabled")
	}
	return nil
}

func foreignKeyCheck(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("run sqlite foreign_key_check: %w", err)
	}
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		var table string
		var rowID sql.NullInt64
		var parent string
		var fkIndex int
		if err := rows.Scan(&table, &rowID, &parent, &fkIndex); err != nil {
			return fmt.Errorf("scan sqlite foreign_key_check: %w", err)
		}
		row := "without-rowid"
		if rowID.Valid {
			row = fmt.Sprintf("%d", rowID.Int64)
		}
		return fmt.Errorf("sqlite foreign_key_check failed: table=%s rowid=%s parent=%s fk=%d", table, row, parent, fkIndex)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("run sqlite foreign_key_check: %w", err)
	}
	return nil
}

func (s *Store) missingTable(ctx context.Context, table string) (bool, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM sqlite_master
WHERE type = 'table' AND name = ?;
`, table).Scan(&count); err != nil {
		return false, fmt.Errorf("check sqlite schema table %s: %w", table, err)
	}
	return count == 0, nil
}

func (s *Store) WithTx(ctx context.Context, fn func(*Tx) error) error {
	workspaceID, err := s.WorkspaceID(ctx)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	// CODE-03: defer rollback so a panic inside fn returns the connection to
	// the single-connection pool. On success Commit makes the deferred
	// Rollback a harmless no-op (sql.ErrTxDone).
	defer func() { _ = tx.Rollback() }()
	wrapped := &Tx{tx: tx, workspaceID: workspaceID}
	if err := fn(wrapped); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) WorkspaceID(ctx context.Context) (string, error) {
	s.workspaceMu.RLock()
	workspaceID := s.workspaceID
	s.workspaceMu.RUnlock()
	if workspaceID != "" {
		return workspaceID, nil
	}
	workspaceID, err := currentWorkspaceID(ctx, s.db)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotInitialized
		}
		if missing, checkErr := s.missingTable(ctx, "workspaces"); checkErr == nil && missing {
			return "", ErrNotInitialized
		}
		return "", fmt.Errorf("read workspace id: %w", err)
	}
	s.workspaceMu.Lock()
	if s.workspaceID == "" {
		s.workspaceID = workspaceID
	}
	workspaceID = s.workspaceID
	s.workspaceMu.Unlock()
	return workspaceID, nil
}

func currentWorkspaceID(ctx context.Context, queryer sqlExecutor) (string, error) {
	var workspaceID string
	err := queryer.QueryRowContext(ctx, `
SELECT id
FROM workspaces
ORDER BY created_at
LIMIT 1;
`).Scan(&workspaceID)
	if err != nil {
		return "", err
	}
	return workspaceID, nil
}

func (s *Store) EnsureWorkspace(ctx context.Context, name, rootPath string) error {
	now := timestampNow()
	workspaceID, err := currentWorkspaceID(ctx, s.db)
	if errors.Is(err, sql.ErrNoRows) {
		workspaceID, err = id.New("ws")
		if err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, `
INSERT OR IGNORE INTO workspaces (id, name, root_path, created_at, updated_at)
VALUES (?, ?, ?, ?, ?);
`, workspaceID, name, rootPath, now, now); err != nil {
			return fmt.Errorf("create workspace: %w", err)
		}
		workspaceID, err = currentWorkspaceID(ctx, s.db)
		if err != nil {
			return fmt.Errorf("read created workspace: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("read workspace: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `
UPDATE workspaces
SET name = ?, root_path = ?, updated_at = ?
WHERE id = ?;
`, name, rootPath, now, workspaceID)
	if err != nil {
		return fmt.Errorf("ensure workspace: %w", err)
	}
	s.workspaceMu.Lock()
	s.workspaceID = workspaceID
	s.workspaceMu.Unlock()
	return nil
}

func (s *Store) EnsureDevice(ctx context.Context, name string) (Device, error) {
	runtimeInfo := platform.Runtime()
	if name == "" {
		if host, err := os.Hostname(); err == nil {
			name = host
		}
	}
	if name == "" {
		name = "local"
	}
	var existing Device
	row := s.db.QueryRowContext(ctx, `
SELECT id, name, os, arch, COALESCE(hostname, ''), COALESCE(public_key, ''), COALESCE(signing_public_key, ''), trust_state
FROM devices
WHERE trust_state = 'local'
ORDER BY created_at
LIMIT 1;
`)
	if err := row.Scan(&existing.ID, &existing.Name, &existing.OS, &existing.Arch, &existing.Hostname, &existing.PublicKey, &existing.SigningPublicKey, &existing.TrustState); err == nil {
		now := timestampNow()
		_, err = s.db.ExecContext(ctx, `
UPDATE devices
SET name = ?, os = ?, arch = ?, hostname = ?, last_seen_at = ?, updated_at = ?
WHERE id = ?;
`, name, runtimeInfo.OS, runtimeInfo.Arch, name, now, now, existing.ID)
		if err != nil {
			return Device{}, fmt.Errorf("update local device: %w", err)
		}
		existing.Name = name
		existing.OS = runtimeInfo.OS
		existing.Arch = runtimeInfo.Arch
		existing.Hostname = name
		return existing, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		if missing, checkErr := s.missingTable(ctx, "devices"); checkErr == nil && missing {
			return Device{}, ErrNotInitialized
		}
		return Device{}, fmt.Errorf("read local device: %w", err)
	}
	deviceID, err := id.New("dev")
	if err != nil {
		return Device{}, err
	}
	now := timestampNow()
	_, err = s.db.ExecContext(ctx, `
INSERT INTO devices (id, name, os, arch, hostname, trust_state, last_seen_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, 'local', ?, ?, ?);
`, deviceID, name, runtimeInfo.OS, runtimeInfo.Arch, name, now, now, now)
	if err != nil {
		return Device{}, fmt.Errorf("create local device: %w", err)
	}
	return Device{ID: deviceID, Name: name, OS: runtimeInfo.OS, Arch: runtimeInfo.Arch, Hostname: name, TrustState: "local"}, nil
}

func (s *Store) CurrentDevice(ctx context.Context) (Device, error) {
	d, err := currentDevice(ctx, s.db)
	if err == nil {
		return d, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, ErrNotInitialized
	}
	if missing, checkErr := s.missingTable(ctx, "devices"); checkErr == nil && missing {
		return Device{}, ErrNotInitialized
	}
	return Device{}, fmt.Errorf("read current device: %w", err)
}

func currentDevice(ctx context.Context, queryer sqlExecutor) (Device, error) {
	var d Device
	row := queryer.QueryRowContext(ctx, `
SELECT id, name, os, arch, COALESCE(hostname, ''), COALESCE(public_key, ''), COALESCE(signing_public_key, ''), trust_state
FROM devices
WHERE trust_state = 'local'
ORDER BY created_at
LIMIT 1;
`)
	if err := row.Scan(&d.ID, &d.Name, &d.OS, &d.Arch, &d.Hostname, &d.PublicKey, &d.SigningPublicKey, &d.TrustState); err != nil {
		return Device{}, err
	}
	return d, nil
}

func (s *Store) ListDevices(ctx context.Context) ([]Device, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, name, os, arch, COALESCE(hostname, ''), COALESCE(public_key, ''), COALESCE(signing_public_key, ''), trust_state
FROM devices
ORDER BY trust_state = 'local' DESC, name, id;
`)
	if err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}

	defer func() { _ = rows.Close() }()
	var devices []Device
	for rows.Next() {
		var d Device
		if err := rows.Scan(&d.ID, &d.Name, &d.OS, &d.Arch, &d.Hostname, &d.PublicKey, &d.SigningPublicKey, &d.TrustState); err != nil {
			return nil, fmt.Errorf("scan device: %w", err)
		}
		devices = append(devices, d)
	}
	return devices, rows.Err()
}

func (s *Store) UpsertDevice(ctx context.Context, device Device) error {
	if device.ID == "" || device.Name == "" || device.OS == "" || device.Arch == "" {
		return fmt.Errorf("device id, name, os, and arch are required")
	}
	switch device.TrustState {
	case "pending", "approved", "revoked", "lost":
	default:
		return fmt.Errorf("unsupported trust state %q", device.TrustState)
	}
	current, err := currentDevice(ctx, s.db)
	if err != nil {
		return err
	}
	if device.ID == current.ID {
		return fmt.Errorf("refusing to enroll current local device")
	}
	now := timestampNow()
	_, err = s.db.ExecContext(ctx, `
INSERT INTO devices (id, name, os, arch, hostname, public_key, signing_public_key, trust_state, last_seen_at, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  name = excluded.name,
  os = excluded.os,
  arch = excluded.arch,
  hostname = excluded.hostname,
  public_key = excluded.public_key,
  signing_public_key = excluded.signing_public_key,
  trust_state = excluded.trust_state,
  updated_at = excluded.updated_at
WHERE devices.trust_state != 'local';
`, device.ID, device.Name, device.OS, device.Arch, device.Hostname, nullEmpty(device.PublicKey), nullEmpty(device.SigningPublicKey), device.TrustState, now, now)
	if err != nil {
		return fmt.Errorf("upsert device: %w", err)
	}
	return nil
}

func (s *Store) RenameDevice(ctx context.Context, deviceID, name string) error {
	if deviceID == "" || name == "" {
		return fmt.Errorf("device id and name are required")
	}
	now := timestampNow()
	result, err := s.db.ExecContext(ctx, `
UPDATE devices SET name = ?, updated_at = ? WHERE id = ?;
`, name, now, deviceID)
	if err != nil {
		return fmt.Errorf("rename device: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read rename device count: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("unknown device %q", deviceID)
	}
	return nil
}

func (s *Store) SetDeviceTrustState(ctx context.Context, deviceID, trustState string) error {
	if deviceID == "" {
		return fmt.Errorf("device id is required")
	}
	switch trustState {
	case "approved", "revoked", "lost", "pending":
	default:
		return fmt.Errorf("unsupported trust state %q", trustState)
	}
	current, err := currentDevice(ctx, s.db)
	if err != nil {
		return err
	}
	if deviceID == current.ID {
		return fmt.Errorf("refusing to change local device trust state")
	}
	now := timestampNow()
	result, err := s.db.ExecContext(ctx, `
UPDATE devices SET trust_state = ?, updated_at = ? WHERE id = ?;
`, trustState, now, deviceID)
	if err != nil {
		return fmt.Errorf("set device trust state: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read device trust update count: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("unknown device %q", deviceID)
	}
	return nil
}

func (s *Store) SetDevicePublicKey(ctx context.Context, deviceID, publicKey string) error {
	if deviceID == "" {
		return fmt.Errorf("device id must not be empty")
	}
	if publicKey == "" {
		return fmt.Errorf("device public key must not be empty")
	}
	now := timestampNow()
	_, err := s.db.ExecContext(ctx, `
UPDATE devices
SET public_key = ?, updated_at = ?
WHERE id = ?;
`, publicKey, now, deviceID)
	if err != nil {
		return fmt.Errorf("set device public key: %w", err)
	}
	return nil
}

func (s *Store) SetDeviceSigningPublicKey(ctx context.Context, deviceID, publicKey string) error {
	if deviceID == "" {
		return fmt.Errorf("device id must not be empty")
	}
	if publicKey == "" {
		return fmt.Errorf("device signing public key must not be empty")
	}
	now := timestampNow()
	_, err := s.db.ExecContext(ctx, `
UPDATE devices
SET signing_public_key = ?, updated_at = ?
WHERE id = ?;
`, publicKey, now, deviceID)
	if err != nil {
		return fmt.Errorf("set device signing public key: %w", err)
	}
	return nil
}

func (s *Store) Summary(ctx context.Context) (Summary, error) {
	workspaceID, err := s.WorkspaceID(ctx)
	if err != nil {
		return Summary{}, err
	}
	var summary Summary
	row := s.db.QueryRowContext(ctx, `
SELECT w.name, w.root_path, COUNT(n.id)
FROM workspaces w
LEFT JOIN namespace_entries n ON n.workspace_id = w.id
WHERE w.id = ?
GROUP BY w.id, w.name, w.root_path;
`, workspaceID)
	if err := row.Scan(&summary.WorkspaceName, &summary.RootPath, &summary.ProjectCount); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Summary{}, ErrNotInitialized
		}
		if missing, checkErr := s.missingTable(ctx, "workspaces"); checkErr == nil && missing {
			return Summary{}, ErrNotInitialized
		}
		return Summary{}, fmt.Errorf("read workspace summary: %w", err)
	}
	if d, err := s.CurrentDevice(ctx); err == nil {
		summary.DeviceID = d.ID
		projects, err := s.ListProjects(ctx)
		if err != nil {
			return Summary{}, err
		}
		summary.Projects = projects
	}
	return summary, nil
}

func (s *Store) UpsertProject(ctx context.Context, params UpsertProjectParams) (NamespaceEntry, error) {
	pk, err := pathkey.Clean(params.Path)
	if err != nil {
		return NamespaceEntry{}, err
	}
	if params.Type == "" {
		params.Type = "plain_folder"
	}
	if params.MaterializationPolicy == "" {
		params.MaterializationPolicy = "lazy"
	}
	if params.DefaultBranch == "" {
		params.DefaultBranch = "main"
	}
	if params.MaterializationState == "" {
		params.MaterializationState = "skeleton"
	}
	if params.DirtyState == "" {
		params.DirtyState = "unknown"
	}
	device, err := s.CurrentDevice(ctx)
	if err != nil {
		return NamespaceEntry{}, err
	}
	workspaceID, err := s.WorkspaceID(ctx)
	if err != nil {
		return NamespaceEntry{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return NamespaceEntry{}, fmt.Errorf("begin project transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	ns, err := upsertNamespaceTx(ctx, tx, workspaceID, pk, params)
	if err != nil {
		return NamespaceEntry{}, err
	}
	now := timestampNow()
	switch params.Type {
	case "git_repo":
		_, err = tx.ExecContext(ctx, `
INSERT INTO git_repos (namespace_id, remote_url, remote_key, default_branch, clone_filter, lfs_policy, forge_kind, created_at, updated_at)
VALUES (?, ?, ?, ?, 'blob:none', COALESCE(NULLIF(?, ''), 'auto'), ?, ?, ?)
ON CONFLICT(namespace_id) DO UPDATE SET
  remote_url = excluded.remote_url,
  remote_key = excluded.remote_key,
  default_branch = excluded.default_branch,
  lfs_policy = CASE WHEN ? != '' THEN excluded.lfs_policy ELSE git_repos.lfs_policy END,
  forge_kind = CASE WHEN excluded.forge_kind != '' THEN excluded.forge_kind ELSE git_repos.forge_kind END,
  updated_at = excluded.updated_at;
`, ns.ID, params.RemoteURL, params.RemoteKey, params.DefaultBranch, params.LFSPolicy, params.ForgeKind, now, now, params.LFSPolicy)
		if err != nil {
			return NamespaceEntry{}, fmt.Errorf("upsert git repo: %w", err)
		}
	case "draft_project":
		_, err = tx.ExecContext(ctx, `
INSERT INTO draft_projects (namespace_id, created_at, updated_at)
VALUES (?, ?, ?)
ON CONFLICT(namespace_id) DO UPDATE SET updated_at = excluded.updated_at;
`, ns.ID, now, now)
		if err != nil {
			return NamespaceEntry{}, fmt.Errorf("upsert draft project: %w", err)
		}
	}
	if params.LocalPath != "" {
		_, err = tx.ExecContext(ctx, `
INSERT INTO device_project_state (device_id, namespace_id, local_path, materialization_state, dirty_state, last_scan_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(device_id, namespace_id) DO UPDATE SET
  local_path = excluded.local_path,
  materialization_state = excluded.materialization_state,
  dirty_state = excluded.dirty_state,
  last_scan_at = excluded.last_scan_at,
  updated_at = excluded.updated_at;
`, device.ID, ns.ID, params.LocalPath, params.MaterializationState, params.DirtyState, now, now)
		if err != nil {
			return NamespaceEntry{}, fmt.Errorf("upsert device project state: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return NamespaceEntry{}, fmt.Errorf("commit project transaction: %w", err)
	}
	return ns, nil
}

func (tx *Tx) UpsertProject(ctx context.Context, params UpsertProjectParams) (NamespaceEntry, error) {
	pk, err := pathkey.Clean(params.Path)
	if err != nil {
		return NamespaceEntry{}, err
	}
	if params.Type == "" {
		params.Type = "plain_folder"
	}
	if params.MaterializationPolicy == "" {
		params.MaterializationPolicy = "lazy"
	}
	if params.DefaultBranch == "" {
		params.DefaultBranch = "main"
	}
	ns, err := upsertNamespaceTx(ctx, tx.tx, tx.workspaceID, pk, params)
	if err != nil {
		return NamespaceEntry{}, err
	}
	now := timestampNow()
	switch params.Type {
	case "git_repo":
		_, err = tx.tx.ExecContext(ctx, `
INSERT INTO git_repos (namespace_id, remote_url, remote_key, default_branch, clone_filter, lfs_policy, forge_kind, created_at, updated_at)
VALUES (?, ?, ?, ?, 'blob:none', COALESCE(NULLIF(?, ''), 'auto'), ?, ?, ?)
ON CONFLICT(namespace_id) DO UPDATE SET
  remote_url = excluded.remote_url,
  remote_key = excluded.remote_key,
  default_branch = excluded.default_branch,
  lfs_policy = CASE WHEN ? != '' THEN excluded.lfs_policy ELSE git_repos.lfs_policy END,
  forge_kind = CASE WHEN excluded.forge_kind != '' THEN excluded.forge_kind ELSE git_repos.forge_kind END,
  updated_at = excluded.updated_at;
`, ns.ID, params.RemoteURL, params.RemoteKey, params.DefaultBranch, params.LFSPolicy, params.ForgeKind, now, now, params.LFSPolicy)
		if err != nil {
			return NamespaceEntry{}, fmt.Errorf("upsert git repo: %w", err)
		}
	case "draft_project":
		_, err = tx.tx.ExecContext(ctx, `
INSERT INTO draft_projects (namespace_id, created_at, updated_at)
VALUES (?, ?, ?)
ON CONFLICT(namespace_id) DO UPDATE SET updated_at = excluded.updated_at;
`, ns.ID, now, now)
		if err != nil {
			return NamespaceEntry{}, fmt.Errorf("upsert draft project: %w", err)
		}
	}
	return ns, nil
}

func upsertNamespaceTx(ctx context.Context, tx *sql.Tx, workspaceID string, pk pathkey.Path, params UpsertProjectParams) (NamespaceEntry, error) {
	now := timestampNow()
	var existingID string
	err := tx.QueryRowContext(ctx, `
SELECT id FROM namespace_entries WHERE workspace_id = ? AND path_key = ?;
`, workspaceID, pk.Key).Scan(&existingID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return NamespaceEntry{}, fmt.Errorf("read namespace entry: %w", err)
	}
	if existingID == "" {
		existingID, err = id.New("prj")
		if err != nil {
			return NamespaceEntry{}, err
		}
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO namespace_entries (
  id, workspace_id, path, path_key, type, display_name, materialization_policy, status,
  source_event_hlc, source_event_device_id, source_event_id,
  created_at, updated_at
)
VALUES (?, ?, ?, ?, ?, ?, ?, 'active', ?, ?, ?, ?, ?)
ON CONFLICT(workspace_id, path_key) DO UPDATE SET
  path = excluded.path,
  type = excluded.type,
  display_name = excluded.display_name,
  materialization_policy = excluded.materialization_policy,
  status = 'active',
  tombstone_hlc = NULL,
  source_event_hlc = COALESCE(excluded.source_event_hlc, namespace_entries.source_event_hlc),
  source_event_device_id = COALESCE(excluded.source_event_device_id, namespace_entries.source_event_device_id),
  source_event_id = COALESCE(excluded.source_event_id, namespace_entries.source_event_id),
  updated_at = excluded.updated_at;
`, existingID, workspaceID, pk.Display, pk.Key, params.Type, filepathBaseSlash(pk.Display), params.MaterializationPolicy,
		nullZero(params.SourceEventHLC), nullEmpty(params.SourceEventDeviceID), nullEmpty(params.SourceEventID), now, now)
	if err != nil {
		return NamespaceEntry{}, fmt.Errorf("upsert namespace entry: %w", err)
	}
	return NamespaceEntry{
		ID: existingID, Path: pk.Display, PathKey: pk.Key, Type: params.Type,
		DisplayName: filepathBaseSlash(pk.Display), MaterializationPolicy: params.MaterializationPolicy, Status: "active",
		SourceEventHLC: params.SourceEventHLC, SourceEventDeviceID: params.SourceEventDeviceID, SourceEventID: params.SourceEventID,
	}, nil
}

func (tx *Tx) TombstoneProject(ctx context.Context, path string, hlc int64) error {
	pk, err := pathkey.Clean(path)
	if err != nil {
		return err
	}
	params := UpsertProjectParams{
		Path:                  pk.Display,
		Type:                  "git_repo",
		MaterializationPolicy: "lazy",
	}
	ns, err := upsertNamespaceTx(ctx, tx.tx, tx.workspaceID, pk, params)
	if err != nil {
		return err
	}
	now := timestampNow()
	_, err = tx.tx.ExecContext(ctx, `
UPDATE namespace_entries
SET status = 'deleted',
    tombstone_hlc = CASE
      WHEN tombstone_hlc IS NULL OR tombstone_hlc < ? THEN ?
      ELSE tombstone_hlc
    END,
    updated_at = ?
WHERE id = ?;
`, hlc, hlc, now, ns.ID)
	if err != nil {
		return fmt.Errorf("tombstone project: %w", err)
	}
	return nil
}

func (tx *Tx) TombstoneHLC(ctx context.Context, path string) (int64, bool, error) {
	pk, err := pathkey.Clean(path)
	if err != nil {
		return 0, false, err
	}
	var tombstone sql.NullInt64
	err = tx.tx.QueryRowContext(ctx, `
SELECT tombstone_hlc
FROM namespace_entries
WHERE workspace_id = ? AND path_key = ? AND status = 'deleted';
`, tx.workspaceID, pk.Key).Scan(&tombstone)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read tombstone: %w", err)
	}
	if !tombstone.Valid {
		return 0, false, nil
	}
	return tombstone.Int64, true, nil
}

// RenameOutcome reports how a project.renamed event was applied.
type RenameOutcome int

const (
	// RenameApplied means the namespace entry was moved to the new path.
	RenameApplied RenameOutcome = iota
	// RenameSourceMissing means no active entry existed at the old path.
	RenameSourceMissing
	// RenameTargetConflict means the new path is already an active, distinct entry.
	RenameTargetConflict
	// RenameStale means the new path holds a newer tombstone (lost-delete guard).
	RenameStale
)

// RenameProject moves an active namespace entry from oldPath to newPath,
// re-keying path_key. It returns a RenameOutcome so the caller can record a
// conflict on a target collision rather than overwriting.
func (tx *Tx) RenameProject(ctx context.Context, oldPath, newPath string, event Event) (RenameOutcome, error) {
	oldPK, err := pathkey.Clean(oldPath)
	if err != nil {
		return 0, err
	}
	newPK, err := pathkey.Clean(newPath)
	if err != nil {
		return 0, err
	}
	if oldPK.Key == newPK.Key {
		return RenameApplied, nil
	}
	var srcID, srcStatus string
	err = tx.tx.QueryRowContext(ctx, `
SELECT id, status FROM namespace_entries WHERE workspace_id = ? AND path_key = ?;
`, tx.workspaceID, oldPK.Key).Scan(&srcID, &srcStatus)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && srcStatus != "active") {
		return RenameSourceMissing, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read rename source: %w", err)
	}
	var tgtID, tgtStatus string
	var tgtTomb sql.NullInt64
	err = tx.tx.QueryRowContext(ctx, `
SELECT id, status, tombstone_hlc FROM namespace_entries WHERE workspace_id = ? AND path_key = ?;
`, tx.workspaceID, newPK.Key).Scan(&tgtID, &tgtStatus, &tgtTomb)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Target free.
	case err != nil:
		return 0, fmt.Errorf("read rename target: %w", err)
	case tgtID == srcID:
		return RenameApplied, nil
	case tgtStatus == "active":
		return RenameTargetConflict, nil
	default: // tombstone at target
		if tgtTomb.Valid && event.HLC <= tgtTomb.Int64 {
			return RenameStale, nil
		}
		if _, err := tx.tx.ExecContext(ctx, `DELETE FROM namespace_entries WHERE id = ?;`, tgtID); err != nil {
			return 0, fmt.Errorf("clear rename target tombstone: %w", err)
		}
	}
	now := timestampNow()
	_, err = tx.tx.ExecContext(ctx, `
UPDATE namespace_entries
SET path = ?, path_key = ?, display_name = ?, status = 'active', tombstone_hlc = NULL,
    source_event_hlc = ?, source_event_device_id = ?, source_event_id = ?, updated_at = ?
WHERE id = ?;
`, newPK.Display, newPK.Key, filepathBaseSlash(newPK.Display),
		nullZero(event.HLC), nullEmpty(event.DeviceID), nullEmpty(event.ID), now, srcID)
	if err != nil {
		return 0, fmt.Errorf("apply rename: %w", err)
	}
	// P5-SYNC-03: leave a tombstone at the old path stamped with the rename
	// HLC. The source row was re-keyed in place, so without this the old
	// path_key vanishes with no tombstone and a stale or cross-batch add/update
	// targeting the old path (even one with a lower HLC) would find no active
	// row and no tombstone, then resurrect a ghost project. The tombstone makes
	// renamed-away paths HLC-gated by the same TombstoneHLC guard as deletes; a
	// legitimately newer add/update at the old path still re-creates it.
	if err := tx.TombstoneProject(ctx, oldPK.Display, event.HLC); err != nil {
		return 0, fmt.Errorf("tombstone renamed-away path: %w", err)
	}
	return RenameApplied, nil
}

// ReceiveRemoteHLC advances the local device clock to be causally after a
// received remote HLC (standard HLC receive rule), so future local events sort
// after events already observed. It assumes the caller has already rejected
// remote timestamps beyond the trusted skew.
func (tx *Tx) ReceiveRemoteHLC(ctx context.Context, remoteHLC int64) error {
	device, err := currentDevice(ctx, tx.tx)
	if err != nil {
		return err
	}
	var lastHLC, nextSeq int64
	err = tx.tx.QueryRowContext(ctx, `
SELECT last_hlc, next_seq FROM device_sync_state WHERE device_id = ?;
`, device.ID).Scan(&lastHLC, &nextSeq)
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.tx.QueryRowContext(ctx, `
SELECT COALESCE(MAX(hlc), 0), COALESCE(MAX(seq), 0) + 1 FROM events WHERE device_id = ?;
`, device.ID).Scan(&lastHLC, &nextSeq); err != nil {
			return fmt.Errorf("seed local clock on receive: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("read local clock on receive: %w", err)
	}
	if nextSeq < 1 {
		nextSeq = 1
	}
	updated := receiveHLC(lastHLC, remoteHLC, time.Now().UTC())
	if updated <= lastHLC {
		return nil
	}
	now := timestampNow()
	_, err = tx.tx.ExecContext(ctx, `
INSERT INTO device_sync_state (device_id, last_hlc, next_seq, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(device_id) DO UPDATE SET last_hlc = excluded.last_hlc, updated_at = excluded.updated_at;
`, device.ID, updated, nextSeq, now)
	if err != nil {
		return fmt.Errorf("advance local clock on receive: %w", err)
	}
	return nil
}

// RecordDraftSnapshotTx records a draft bundle snapshot within the current
// transaction (DRAFT-02). It ensures a draft_projects row exists, inserts the
// snapshot (idempotent on source_event_id), and points
// draft_projects.current_snapshot_id at it.
func (tx *Tx) RecordDraftSnapshotTx(ctx context.Context, namespaceID, blobRef string, byteSize, fileCount int64, event Event) error {
	if !strings.HasPrefix(blobRef, "age_blob:") {
		return fmt.Errorf("draft blob ref must use age_blob: prefix")
	}
	now := timestampNow()
	if _, err := tx.tx.ExecContext(ctx, `
INSERT OR IGNORE INTO draft_projects (namespace_id, max_bytes, max_files, created_at, updated_at)
VALUES (?, 104857600, 5000, ?, ?);
`, namespaceID, now, now); err != nil {
		return fmt.Errorf("ensure draft project: %w", err)
	}
	var existing string
	_ = tx.tx.QueryRowContext(ctx, `
SELECT id FROM draft_snapshots WHERE namespace_id = ? AND source_event_id = ?;
`, namespaceID, event.ID).Scan(&existing)
	if existing != "" {
		return nil // idempotent
	}
	snapID, err := id.New("snap")
	if err != nil {
		return err
	}
	if _, err := tx.tx.ExecContext(ctx, `
INSERT INTO draft_snapshots (id, namespace_id, blob_ref, byte_size, file_count, source_event_hlc, source_event_device_id, source_event_id, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);
`, snapID, namespaceID, blobRef, byteSize, fileCount, event.HLC, event.DeviceID, event.ID, now); err != nil {
		return fmt.Errorf("insert draft snapshot: %w", err)
	}
	if _, err := tx.tx.ExecContext(ctx, `
UPDATE draft_projects SET current_snapshot_id = ?, updated_at = ? WHERE namespace_id = ?;
`, snapID, now, namespaceID); err != nil {
		return fmt.Errorf("update draft current snapshot: %w", err)
	}
	return nil
}

// GCTombstones permanently removes deleted namespace entries whose tombstone
// HLC is strictly below beforeHLC. Callers must pass the minimum HLC that every
// approved sync cursor has already passed, so no peer can still resurrect the
// entry with a stale add. Returns the number of rows purged.
func (s *Store) GCTombstones(ctx context.Context, beforeHLC int64) (int, error) {
	workspaceID, err := s.WorkspaceID(ctx)
	if err != nil {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx, `
DELETE FROM namespace_entries
WHERE workspace_id = ? AND status = 'deleted' AND tombstone_hlc IS NOT NULL AND tombstone_hlc < ?;
`, workspaceID, beforeHLC)
	if err != nil {
		return 0, fmt.Errorf("gc tombstones: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("gc tombstones rows: %w", err)
	}
	return int(n), nil
}

func receiveHLC(last, remote int64, now time.Time) int64 {
	nowPhysical := now.UnixMilli()
	lastPhysical, lastLogical := unpackHLC(last)
	remotePhysical, remoteLogical := unpackHLC(remote)
	maxPhysical := nowPhysical
	if lastPhysical > maxPhysical {
		maxPhysical = lastPhysical
	}
	if remotePhysical > maxPhysical {
		maxPhysical = remotePhysical
	}
	var logical int64
	switch {
	case maxPhysical == lastPhysical && maxPhysical == remotePhysical:
		if lastLogical > remoteLogical {
			logical = lastLogical + 1
		} else {
			logical = remoteLogical + 1
		}
	case maxPhysical == lastPhysical:
		logical = lastLogical + 1
	case maxPhysical == remotePhysical:
		logical = remoteLogical + 1
	default:
		logical = 0
	}
	if logical > hlcLogicalMask {
		maxPhysical++
		logical = 0
	}
	return packHLC(maxPhysical, logical)
}

func (s *Store) ProjectByPath(ctx context.Context, path string) (ProjectStatus, error) {
	pk, err := pathkey.Clean(path)
	if err != nil {
		return ProjectStatus{}, err
	}
	workspaceID, err := s.WorkspaceID(ctx)
	if err != nil {
		return ProjectStatus{}, err
	}
	return projectByPath(ctx, s.db, workspaceID, pk)
}

func (s *Store) ProjectByID(ctx context.Context, id string) (ProjectStatus, error) {
	workspaceID, err := s.WorkspaceID(ctx)
	if err != nil {
		return ProjectStatus{}, err
	}
	return projectByID(ctx, s.db, workspaceID, id)
}

func (tx *Tx) ProjectByPath(ctx context.Context, path string) (ProjectStatus, error) {
	pk, err := pathkey.Clean(path)
	if err != nil {
		return ProjectStatus{}, err
	}
	return projectByPath(ctx, tx.tx, tx.workspaceID, pk)
}

func projectByPath(ctx context.Context, queryer sqlExecutor, workspaceID string, pk pathkey.Path) (ProjectStatus, error) {
	row := queryer.QueryRowContext(ctx, `
SELECT n.id, n.path, n.path_key, n.type, COALESCE(n.display_name, ''), n.materialization_policy, n.status,
       COALESCE(n.source_event_hlc, 0), COALESCE(n.source_event_device_id, ''), COALESCE(n.source_event_id, ''),
       COALESCE(g.remote_url, ''), COALESCE(g.remote_key, ''), COALESCE(g.default_branch, ''), COALESCE(g.lfs_policy, ''), COALESCE(g.forge_kind, ''),
       COALESCE(dps.local_path, ''), COALESCE(dps.materialization_state, ''), COALESCE(dps.dirty_state, '')
FROM namespace_entries n
LEFT JOIN git_repos g ON g.namespace_id = n.id
LEFT JOIN devices d ON d.trust_state = 'local'
LEFT JOIN device_project_state dps ON dps.namespace_id = n.id AND dps.device_id = d.id
WHERE n.workspace_id = ? AND n.path_key = ? AND n.status = 'active';
	`, workspaceID, pk.Key)
	var p ProjectStatus
	err := row.Scan(&p.ID, &p.Path, &p.PathKey, &p.Type, &p.DisplayName, &p.MaterializationPolicy, &p.Status,
		&p.SourceEventHLC, &p.SourceEventDeviceID, &p.SourceEventID,
		&p.RemoteURL, &p.RemoteKey, &p.DefaultBranch, &p.LFSPolicy, &p.ForgeKind, &p.LocalPath, &p.MaterializationState, &p.DirtyState)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ProjectStatus{}, fmt.Errorf("unknown namespace path %q", pk.Display)
		}
		return ProjectStatus{}, fmt.Errorf("read project: %w", err)
	}
	return p, nil
}

func projectByID(ctx context.Context, queryer sqlExecutor, workspaceID, id string) (ProjectStatus, error) {
	row := queryer.QueryRowContext(ctx, `
SELECT n.id, n.path, n.path_key, n.type, COALESCE(n.display_name, ''), n.materialization_policy, n.status,
       COALESCE(n.source_event_hlc, 0), COALESCE(n.source_event_device_id, ''), COALESCE(n.source_event_id, ''),
       COALESCE(g.remote_url, ''), COALESCE(g.remote_key, ''), COALESCE(g.default_branch, ''), COALESCE(g.lfs_policy, ''), COALESCE(g.forge_kind, ''),
       COALESCE(dps.local_path, ''), COALESCE(dps.materialization_state, ''), COALESCE(dps.dirty_state, '')
FROM namespace_entries n
LEFT JOIN git_repos g ON g.namespace_id = n.id
LEFT JOIN devices d ON d.trust_state = 'local'
LEFT JOIN device_project_state dps ON dps.namespace_id = n.id AND dps.device_id = d.id
WHERE n.workspace_id = ? AND n.id = ? AND n.status = 'active';
	`, workspaceID, id)
	var p ProjectStatus
	err := row.Scan(&p.ID, &p.Path, &p.PathKey, &p.Type, &p.DisplayName, &p.MaterializationPolicy, &p.Status,
		&p.SourceEventHLC, &p.SourceEventDeviceID, &p.SourceEventID,
		&p.RemoteURL, &p.RemoteKey, &p.DefaultBranch, &p.LFSPolicy, &p.ForgeKind, &p.LocalPath, &p.MaterializationState, &p.DirtyState)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ProjectStatus{}, fmt.Errorf("unknown namespace id %q", id)
		}
		return ProjectStatus{}, fmt.Errorf("read project by id: %w", err)
	}
	return p, nil
}

func (s *Store) ListProjects(ctx context.Context) ([]ProjectStatus, error) {
	workspaceID, err := s.WorkspaceID(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT n.id, n.path, n.path_key, n.type, COALESCE(n.display_name, ''), n.materialization_policy, n.status,
       COALESCE(n.source_event_hlc, 0), COALESCE(n.source_event_device_id, ''), COALESCE(n.source_event_id, ''),
       COALESCE(g.remote_url, ''), COALESCE(g.remote_key, ''), COALESCE(g.default_branch, ''), COALESCE(g.lfs_policy, ''), COALESCE(g.forge_kind, ''),
       COALESCE(dps.local_path, ''), COALESCE(dps.materialization_state, ''), COALESCE(dps.dirty_state, '')
FROM namespace_entries n
LEFT JOIN git_repos g ON g.namespace_id = n.id
LEFT JOIN devices d ON d.trust_state = 'local'
LEFT JOIN device_project_state dps ON dps.namespace_id = n.id AND dps.device_id = d.id
WHERE n.workspace_id = ? AND n.status = 'active'
ORDER BY n.path_key;
`, workspaceID)
	if err != nil {
		if missing, checkErr := s.missingTable(ctx, "namespace_entries"); checkErr == nil && missing {
			return nil, ErrNotInitialized
		}
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var projects []ProjectStatus
	for rows.Next() {
		var p ProjectStatus
		if err := rows.Scan(&p.ID, &p.Path, &p.PathKey, &p.Type, &p.DisplayName, &p.MaterializationPolicy, &p.Status,
			&p.SourceEventHLC, &p.SourceEventDeviceID, &p.SourceEventID,
			&p.RemoteURL, &p.RemoteKey, &p.DefaultBranch, &p.LFSPolicy, &p.ForgeKind, &p.LocalPath, &p.MaterializationState, &p.DirtyState); err != nil {
			return nil, fmt.Errorf("scan project: %w", err)
		}
		projects = append(projects, p)
	}
	return projects, rows.Err()
}

func (s *Store) UpdateProjectLocalState(ctx context.Context, namespaceID, localPath, materialization, dirty string) error {
	device, err := s.CurrentDevice(ctx)
	if err != nil {
		return err
	}
	now := timestampNow()
	_, err = s.db.ExecContext(ctx, `
INSERT INTO device_project_state (device_id, namespace_id, local_path, materialization_state, dirty_state, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(device_id, namespace_id) DO UPDATE SET
  local_path = excluded.local_path,
  materialization_state = excluded.materialization_state,
  dirty_state = excluded.dirty_state,
  updated_at = excluded.updated_at;
`, device.ID, namespaceID, localPath, materialization, dirty, now)
	if err != nil {
		return fmt.Errorf("update project local state: %w", err)
	}
	return nil
}

func (s *Store) UpdateGitDefaultBranch(ctx context.Context, namespaceID, branch string) error {
	now := timestampNow()
	if _, err := s.db.ExecContext(ctx, `
UPDATE git_repos SET default_branch = ?, updated_at = ? WHERE namespace_id = ?;
`, branch, now, namespaceID); err != nil {
		return fmt.Errorf("update git default branch: %w", err)
	}
	return nil
}

// SetProjectForgeKind persists a per-project forge override (GIT-05) so a
// self-hosted GitLab/Gitea instance routes to glab/tea instead of degrading to
// a compare URL. An empty kind clears the override (fall back to detection).
func (s *Store) SetProjectForgeKind(ctx context.Context, namespaceID, kind string) error {
	now := timestampNow()
	if _, err := s.db.ExecContext(ctx, `
UPDATE git_repos SET forge_kind = ?, updated_at = ? WHERE namespace_id = ?;
`, kind, now, namespaceID); err != nil {
		return fmt.Errorf("update git forge kind: %w", err)
	}
	return nil
}

func (s *Store) SaveCapturedEnvProfile(ctx context.Context, namespaceID, name string, varNames []string, encryptedValueRef string) (EnvProfile, error) {
	if namespaceID == "" {
		return EnvProfile{}, fmt.Errorf("namespace id must not be empty")
	}
	if name == "" {
		name = "default"
	}
	if len(varNames) == 0 {
		return EnvProfile{}, fmt.Errorf("env profile must contain at least one binding")
	}
	if !strings.HasPrefix(encryptedValueRef, "age_blob:") {
		return EnvProfile{}, fmt.Errorf("encrypted value ref must use age_blob: prefix")
	}
	var profile EnvProfile
	err := s.WithTx(ctx, func(tx *Tx) error {
		var existingID string
		err := tx.tx.QueryRowContext(ctx, `
SELECT COALESCE(env_profile_id, '')
FROM namespace_entries
WHERE id = ? AND workspace_id = ? AND status = 'active';
`, namespaceID, tx.workspaceID).Scan(&existingID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("unknown namespace id %q", namespaceID)
			}
			return fmt.Errorf("read namespace env profile: %w", err)
		}
		now := timestampNow()
		if existingID == "" {
			var err error
			existingID, err = id.New("env")
			if err != nil {
				return err
			}
			if _, err := tx.tx.ExecContext(ctx, `
INSERT INTO env_profiles (id, workspace_id, name, provider, mode, created_at, updated_at)
VALUES (?, ?, ?, 'devstrap_encrypted', 'hydrate_or_runtime', ?, ?);
`, existingID, tx.workspaceID, name, now, now); err != nil {
				return fmt.Errorf("insert env profile: %w", err)
			}
			if _, err := tx.tx.ExecContext(ctx, `
UPDATE namespace_entries SET env_profile_id = ?, updated_at = ? WHERE id = ?;
`, existingID, now, namespaceID); err != nil {
				return fmt.Errorf("attach env profile: %w", err)
			}
		} else {
			if _, err := tx.tx.ExecContext(ctx, `
UPDATE env_profiles
SET name = ?, provider = 'devstrap_encrypted', mode = 'hydrate_or_runtime', updated_at = ?
WHERE id = ?;
`, name, now, existingID); err != nil {
				return fmt.Errorf("update env profile: %w", err)
			}
			if _, err := tx.tx.ExecContext(ctx, `DELETE FROM secret_bindings WHERE env_profile_id = ?;`, existingID); err != nil {
				return fmt.Errorf("replace env bindings: %w", err)
			}
		}
		for _, varName := range varNames {
			bindingID, err := id.New("sec")
			if err != nil {
				return err
			}
			if _, err := tx.tx.ExecContext(ctx, `
INSERT INTO secret_bindings (id, env_profile_id, var_name, encrypted_value_ref, required, created_at, updated_at)
VALUES (?, ?, ?, ?, 1, ?, ?);
`, bindingID, existingID, varName, encryptedValueRef, now, now); err != nil {
				return fmt.Errorf("insert secret binding %s: %w", varName, err)
			}
		}
		profile = EnvProfile{
			ID:          existingID,
			WorkspaceID: tx.workspaceID,
			Name:        name,
			Provider:    "devstrap_encrypted",
			Mode:        "hydrate_or_runtime",
		}
		return nil
	})
	if err != nil {
		return EnvProfile{}, err
	}
	return profile, nil
}

func (s *Store) SaveProviderEnvProfile(ctx context.Context, namespaceID, name, provider string, refs map[string]string) (EnvProfile, error) {
	if namespaceID == "" {
		return EnvProfile{}, fmt.Errorf("namespace id must not be empty")
	}
	if name == "" {
		name = "default"
	}
	if provider == "" {
		return EnvProfile{}, fmt.Errorf("provider must not be empty")
	}
	if len(refs) == 0 {
		return EnvProfile{}, fmt.Errorf("env profile must contain at least one binding")
	}
	varNames := make([]string, 0, len(refs))
	for varName, ref := range refs {
		if varName == "" {
			return EnvProfile{}, fmt.Errorf("env variable name must not be empty")
		}
		if strings.TrimSpace(ref) == "" {
			return EnvProfile{}, fmt.Errorf("provider ref for %s must not be empty", varName)
		}
		varNames = append(varNames, varName)
	}
	sort.Strings(varNames)
	var profile EnvProfile
	err := s.WithTx(ctx, func(tx *Tx) error {
		var existingID string
		err := tx.tx.QueryRowContext(ctx, `
SELECT COALESCE(env_profile_id, '')
FROM namespace_entries
WHERE id = ? AND workspace_id = ? AND status = 'active';
`, namespaceID, tx.workspaceID).Scan(&existingID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("unknown namespace id %q", namespaceID)
			}
			return fmt.Errorf("read namespace env profile: %w", err)
		}
		now := timestampNow()
		if existingID == "" {
			var err error
			existingID, err = id.New("env")
			if err != nil {
				return err
			}
			if _, err := tx.tx.ExecContext(ctx, `
INSERT INTO env_profiles (id, workspace_id, name, provider, mode, created_at, updated_at)
VALUES (?, ?, ?, ?, 'runtime_only', ?, ?);
`, existingID, tx.workspaceID, name, provider, now, now); err != nil {
				return fmt.Errorf("insert provider env profile: %w", err)
			}
			if _, err := tx.tx.ExecContext(ctx, `
UPDATE namespace_entries SET env_profile_id = ?, updated_at = ? WHERE id = ?;
`, existingID, now, namespaceID); err != nil {
				return fmt.Errorf("attach env profile: %w", err)
			}
		} else {
			if _, err := tx.tx.ExecContext(ctx, `
UPDATE env_profiles
SET name = ?, provider = ?, mode = 'runtime_only', updated_at = ?
WHERE id = ?;
`, name, provider, now, existingID); err != nil {
				return fmt.Errorf("update provider env profile: %w", err)
			}
			if _, err := tx.tx.ExecContext(ctx, `DELETE FROM secret_bindings WHERE env_profile_id = ?;`, existingID); err != nil {
				return fmt.Errorf("replace env bindings: %w", err)
			}
		}
		for _, varName := range varNames {
			bindingID, err := id.New("sec")
			if err != nil {
				return err
			}
			if _, err := tx.tx.ExecContext(ctx, `
INSERT INTO secret_bindings (id, env_profile_id, var_name, provider_ref, required, created_at, updated_at)
VALUES (?, ?, ?, ?, 1, ?, ?);
`, bindingID, existingID, varName, refs[varName], now, now); err != nil {
				return fmt.Errorf("insert provider secret binding %s: %w", varName, err)
			}
		}
		profile = EnvProfile{
			ID:          existingID,
			WorkspaceID: tx.workspaceID,
			Name:        name,
			Provider:    provider,
			Mode:        "runtime_only",
		}
		return nil
	})
	if err != nil {
		return EnvProfile{}, err
	}
	return profile, nil
}

func (s *Store) EnvProfileForProject(ctx context.Context, namespaceID string) (EnvProfile, []SecretBinding, error) {
	workspaceID, err := s.WorkspaceID(ctx)
	if err != nil {
		return EnvProfile{}, nil, err
	}
	var profile EnvProfile
	err = s.db.QueryRowContext(ctx, `
SELECT e.id, e.workspace_id, e.name, e.provider, e.mode
FROM namespace_entries n
JOIN env_profiles e ON e.id = n.env_profile_id
WHERE n.id = ? AND n.workspace_id = ? AND n.status = 'active';
`, namespaceID, workspaceID).Scan(&profile.ID, &profile.WorkspaceID, &profile.Name, &profile.Provider, &profile.Mode)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return EnvProfile{}, nil, fmt.Errorf("env profile not found for namespace id %q", namespaceID)
		}
		return EnvProfile{}, nil, fmt.Errorf("read env profile: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, env_profile_id, var_name, COALESCE(provider_ref, ''), COALESCE(encrypted_value_ref, ''), required, needs_rotation
FROM secret_bindings
WHERE env_profile_id = ?
ORDER BY var_name;
`, profile.ID)
	if err != nil {
		return EnvProfile{}, nil, fmt.Errorf("list secret bindings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var bindings []SecretBinding
	for rows.Next() {
		var binding SecretBinding
		var required, needsRotation int
		if err := rows.Scan(&binding.ID, &binding.EnvProfileID, &binding.VarName, &binding.ProviderRef, &binding.EncryptedValueRef, &required, &needsRotation); err != nil {
			return EnvProfile{}, nil, fmt.Errorf("scan secret binding: %w", err)
		}
		binding.Required = required != 0
		binding.NeedsRotation = needsRotation != 0
		bindings = append(bindings, binding)
	}
	return profile, bindings, rows.Err()
}

// MarkEncryptedBindingsNeedingRotation flags every encrypted secret binding as
// requiring value rotation. It is invoked when a device is revoked or marked
// lost: that device could decrypt any blob it was a recipient of, so the values
// must be rotated at their source — rewrapping recipients alone does not revoke
// historical access. Returns the number of bindings flagged.
func (s *Store) MarkEncryptedBindingsNeedingRotation(ctx context.Context) (int, error) {
	now := timestampNow()
	res, err := s.db.ExecContext(ctx, `
UPDATE secret_bindings
SET needs_rotation = 1, updated_at = ?
WHERE encrypted_value_ref IS NOT NULL AND needs_rotation = 0;
`, now)
	if err != nil {
		return 0, fmt.Errorf("flag bindings for rotation: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("flag bindings rows: %w", err)
	}
	return int(n), nil
}

// CountSecretBindingsNeedingRotation reports how many secret values are flagged
// for rotation (e.g. after a device revocation).
func (s *Store) CountSecretBindingsNeedingRotation(ctx context.Context) (int, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM secret_bindings WHERE needs_rotation = 1;
`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count bindings needing rotation: %w", err)
	}
	return count, nil
}

// DraftSnapshot records a content-addressed age_blob bundle for a non-git
// project (DRAFT-02).
type DraftSnapshot struct {
	ID                  string
	NamespaceID         string
	BlobRef             string
	ByteSize            int64
	FileCount           int64
	SourceEventHLC      int64
	SourceEventDeviceID string
	SourceEventID       string
}

// LatestDraftSnapshot returns the most recent draft bundle snapshot for a
// project, or nil with no error when no snapshot exists (DRAFT-02).
func (s *Store) LatestDraftSnapshot(ctx context.Context, namespaceID string) (*DraftSnapshot, error) {
	var snap DraftSnapshot
	err := s.db.QueryRowContext(ctx, `
SELECT id, namespace_id, blob_ref, byte_size, file_count,
       COALESCE(source_event_hlc, 0), COALESCE(source_event_device_id, ''), COALESCE(source_event_id, '')
FROM draft_snapshots
WHERE namespace_id = ?
ORDER BY created_at DESC, id DESC
LIMIT 1;
`, namespaceID).Scan(&snap.ID, &snap.NamespaceID, &snap.BlobRef, &snap.ByteSize, &snap.FileCount,
		&snap.SourceEventHLC, &snap.SourceEventDeviceID, &snap.SourceEventID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read latest draft snapshot: %w", err)
	}
	return &snap, nil
}

// RecordDraftSnapshot inserts a draft bundle snapshot and points the project's
// current_snapshot_id at it (DRAFT-02). It is idempotent on source_event_id:
// re-applying the same event does not create a duplicate.
func (s *Store) RecordDraftSnapshot(ctx context.Context, namespaceID, blobRef string, byteSize, fileCount int64, event Event) error {
	if !strings.HasPrefix(blobRef, "age_blob:") {
		return fmt.Errorf("draft blob ref must use age_blob: prefix")
	}
	snapID, err := id.New("snap")
	if err != nil {
		return err
	}
	now := timestampNow()
	return s.WithTx(ctx, func(tx *Tx) error {
		var existing string
		_ = tx.tx.QueryRowContext(ctx, `
SELECT id FROM draft_snapshots WHERE namespace_id = ? AND source_event_id = ?;
`, namespaceID, event.ID).Scan(&existing)
		if existing != "" {
			return nil // idempotent: this event's snapshot is already recorded
		}
		if _, err := tx.tx.ExecContext(ctx, `
INSERT INTO draft_snapshots (id, namespace_id, blob_ref, byte_size, file_count, source_event_hlc, source_event_device_id, source_event_id, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);
`, snapID, namespaceID, blobRef, byteSize, fileCount, event.HLC, event.DeviceID, event.ID, now); err != nil {
			return fmt.Errorf("insert draft snapshot: %w", err)
		}
		if _, err := tx.tx.ExecContext(ctx, `
UPDATE draft_projects SET current_snapshot_id = ?, updated_at = ? WHERE namespace_id = ?;
`, snapID, now, namespaceID); err != nil {
			return fmt.Errorf("update draft current snapshot: %w", err)
		}
		return nil
	})
}

// DraftProjectLimits returns the per-project max_bytes and max_files for a
// draft project (DRAFT-04). Defaults are applied when no row exists.
func (s *Store) DraftProjectLimits(ctx context.Context, namespaceID string) (int64, int64, error) {
	var maxBytes, maxFiles int64
	err := s.db.QueryRowContext(ctx, `
SELECT max_bytes, max_files FROM draft_projects WHERE namespace_id = ?;
`, namespaceID).Scan(&maxBytes, &maxFiles)
	if errors.Is(err, sql.ErrNoRows) {
		return 104857600, 5000, nil // schema defaults
	}
	if err != nil {
		return 0, 0, fmt.Errorf("read draft project limits: %w", err)
	}
	return maxBytes, maxFiles, nil
}

// EnsureDraftProject creates a draft_projects row for a namespace if one does
// not exist, so limits and snapshot pointers are available (DRAFT-02/04).
func (s *Store) EnsureDraftProject(ctx context.Context, namespaceID string) error {
	now := timestampNow()
	_, err := s.db.ExecContext(ctx, `
INSERT OR IGNORE INTO draft_projects (namespace_id, max_bytes, max_files, created_at, updated_at)
VALUES (?, 104857600, 5000, ?, ?);
`, namespaceID, now, now)
	if err != nil {
		return fmt.Errorf("ensure draft project: %w", err)
	}
	return nil
}

// ApprovedRecipients returns the age recipient public keys for the local device
// plus all approved remote devices (HUB-04 re-encryption recipient set).
func (s *Store) ApprovedRecipients(ctx context.Context) ([]string, error) {
	local, err := s.CurrentDevice(ctx)
	if err != nil {
		return nil, err
	}
	if local.PublicKey == "" {
		return nil, fmt.Errorf("local device has no age recipient; run devstrap init")
	}
	seen := map[string]bool{local.PublicKey: true}
	recipients := []string{local.PublicKey}
	devices, err := s.ListDevices(ctx)
	if err != nil {
		return nil, err
	}
	for _, d := range devices {
		if d.ID == local.ID || d.PublicKey == "" || d.TrustState != "approved" {
			continue
		}
		if !seen[d.PublicKey] {
			recipients = append(recipients, d.PublicKey)
			seen[d.PublicKey] = true
		}
	}
	return recipients, nil
}

// AllBlobRefs returns every distinct age_blob:<sha256> reference in the store
// (env bindings + draft snapshots) (HUB-04/HUB-05). These are the blobs that
// may need rewrapping on device revoke or GC when unreferenced.
func (s *Store) AllBlobRefs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT DISTINCT encrypted_value_ref FROM secret_bindings WHERE encrypted_value_ref IS NOT NULL AND encrypted_value_ref LIKE 'age_blob:%'
UNION
SELECT DISTINCT blob_ref FROM draft_snapshots WHERE blob_ref LIKE 'age_blob:%';
`)
	if err != nil {
		return nil, fmt.Errorf("list blob refs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var refs []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil, fmt.Errorf("scan blob ref: %w", err)
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

// PruneDraftSnapshots deletes superseded draft snapshot rows, keeping the most
// recent `keep` per project (P5-HUB-02). RecordDraftSnapshot only ever INSERTs,
// so without pruning every superseded snapshot keeps its blob "referenced"
// forever and neither local nor hub GC can reclaim a stale draft blob. keep is
// clamped to >= 1 so the current snapshot (highest HLC) is always retained.
// Returns the number of rows pruned.
func (s *Store) PruneDraftSnapshots(ctx context.Context, keep int) (int, error) {
	if keep < 1 {
		keep = 1
	}
	res, err := s.db.ExecContext(ctx, `
DELETE FROM draft_snapshots
WHERE id IN (
  SELECT id FROM (
    SELECT id, ROW_NUMBER() OVER (
      PARTITION BY namespace_id
      ORDER BY COALESCE(source_event_hlc, 0) DESC, created_at DESC, id DESC
    ) AS rn
    FROM draft_snapshots
  ) WHERE rn > ?
);
`, keep)
	if err != nil {
		return 0, fmt.Errorf("prune draft snapshots: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune draft snapshots rows: %w", err)
	}
	return int(n), nil
}

// EnvBlobRefs returns the distinct age_blob refs held by encrypted env bindings
// (P5-SEC-04). These blobs are local-only and are never pushed to the hub, so
// the revoke rewrap must not upload or delete them there.
func (s *Store) EnvBlobRefs(ctx context.Context) ([]string, error) {
	return s.scanRefs(ctx, `SELECT DISTINCT encrypted_value_ref FROM secret_bindings WHERE encrypted_value_ref LIKE 'age_blob:%';`)
}

// DraftBlobRefs returns the distinct age_blob refs held by draft snapshots
// (P5-SEC-04). These are the only blobs synced through the hub.
func (s *Store) DraftBlobRefs(ctx context.Context) ([]string, error) {
	return s.scanRefs(ctx, `SELECT DISTINCT blob_ref FROM draft_snapshots WHERE blob_ref LIKE 'age_blob:%';`)
}

func (s *Store) scanRefs(ctx context.Context, query string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("scan blob refs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var refs []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil, fmt.Errorf("scan blob ref: %w", err)
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

// DraftSnapshotRef is the metadata needed to re-emit a superseding
// draft.snapshot.created event when a draft blob is rewrapped (P5-SEC-01).
type DraftSnapshotRef struct {
	Path      string
	ByteSize  int64
	FileCount int64
}

// DraftSnapshotsForBlobRef returns the (path, size, count) of every active
// draft snapshot referencing ref (P5-SEC-01), so a rewrap can emit a superseding
// event carrying the new ref before the old hub blob is deleted.
func (s *Store) DraftSnapshotsForBlobRef(ctx context.Context, ref string) ([]DraftSnapshotRef, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT n.path, ds.byte_size, ds.file_count
FROM draft_snapshots ds
JOIN namespace_entries n ON n.id = ds.namespace_id
WHERE ds.blob_ref = ?;
`, ref)
	if err != nil {
		return nil, fmt.Errorf("read draft snapshots for blob: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []DraftSnapshotRef
	for rows.Next() {
		var r DraftSnapshotRef
		if err := rows.Scan(&r.Path, &r.ByteSize, &r.FileCount); err != nil {
			return nil, fmt.Errorf("scan draft snapshot ref: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// QueuePendingHubDelete records a blob ref orphaned by a local-only revoke
// rewrap so the next hub-enabled sync/gc deletes it (P5-PROD-02). Idempotent.
func (s *Store) QueuePendingHubDelete(ctx context.Context, ref string) error {
	workspaceID, err := s.WorkspaceID(ctx)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO pending_hub_deletes (workspace_id, blob_ref, queued_at)
VALUES (?, ?, ?)
ON CONFLICT(workspace_id, blob_ref) DO NOTHING;
`, workspaceID, ref, timestampNow())
	if err != nil {
		return fmt.Errorf("queue pending hub delete: %w", err)
	}
	return nil
}

// PendingHubDeletes returns the queued orphaned blob refs (P5-PROD-02).
func (s *Store) PendingHubDeletes(ctx context.Context) ([]string, error) {
	workspaceID, err := s.WorkspaceID(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT blob_ref FROM pending_hub_deletes WHERE workspace_id = ?;`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list pending hub deletes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var refs []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil, fmt.Errorf("scan pending hub delete: %w", err)
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

// ClearPendingHubDelete removes a drained entry from the queue (P5-PROD-02).
func (s *Store) ClearPendingHubDelete(ctx context.Context, ref string) error {
	workspaceID, err := s.WorkspaceID(ctx)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM pending_hub_deletes WHERE workspace_id = ? AND blob_ref = ?;`, workspaceID, ref)
	if err != nil {
		return fmt.Errorf("clear pending hub delete: %w", err)
	}
	return nil
}

// UpdateBlobRef repoints every reference from oldRef to newRef across
// secret_bindings and draft_snapshots (HUB-04 re-encryption).
func (s *Store) UpdateBlobRef(ctx context.Context, oldRef, newRef string) error {
	if !strings.HasPrefix(oldRef, "age_blob:") || !strings.HasPrefix(newRef, "age_blob:") {
		return fmt.Errorf("blob refs must use age_blob: prefix")
	}
	return s.WithTx(ctx, func(tx *Tx) error {
		if _, err := tx.tx.ExecContext(ctx, `
UPDATE secret_bindings SET encrypted_value_ref = ?, updated_at = ? WHERE encrypted_value_ref = ?;
`, newRef, timestampNow(), oldRef); err != nil {
			return fmt.Errorf("update env blob refs: %w", err)
		}
		if _, err := tx.tx.ExecContext(ctx, `
UPDATE draft_snapshots SET blob_ref = ? WHERE blob_ref = ?;
`, newRef, oldRef); err != nil {
			return fmt.Errorf("update draft blob refs: %w", err)
		}
		return nil
	})
}

// BlobRefCount returns a map of age_blob ref → reference count (HUB-05). A blob
// is safe to GC only when its count is zero and it is older than the
// retention/snapshot horizon (the latter gate is deferred until full-state
// snapshot exchange exists).
func (s *Store) BlobRefCount(ctx context.Context) (map[string]int, error) {
	refs, err := s.AllBlobRefs(ctx)
	if err != nil {
		return nil, err
	}
	counts := make(map[string]int)
	for _, ref := range refs {
		counts[ref]++
	}
	return counts, nil
}

func (s *Store) InsertConflict(ctx context.Context, namespaceID, typ, detailsJSON string) error {
	workspaceID, err := s.WorkspaceID(ctx)
	if err != nil {
		return err
	}
	return insertConflict(ctx, s.db, workspaceID, namespaceID, typ, detailsJSON)
}

func (s *Store) CountOpenConflicts(ctx context.Context) (int, error) {
	workspaceID, err := s.WorkspaceID(ctx)
	if err != nil {
		return 0, err
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM conflicts
WHERE workspace_id = ? AND status = 'open';
`, workspaceID).Scan(&count); err != nil {
		return 0, fmt.Errorf("count open conflicts: %w", err)
	}
	return count, nil
}

func (s *Store) OpenConflicts(ctx context.Context) ([]Conflict, error) {
	workspaceID, err := s.WorkspaceID(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, COALESCE(namespace_id, ''), type, status, details_json
FROM conflicts
WHERE workspace_id = ? AND status = 'open'
ORDER BY type, details_json, id;
`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list open conflicts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var conflicts []Conflict
	for rows.Next() {
		var conflict Conflict
		if err := rows.Scan(&conflict.ID, &conflict.NamespaceID, &conflict.Type, &conflict.Status, &conflict.DetailsJSON); err != nil {
			return nil, fmt.Errorf("scan open conflict: %w", err)
		}
		conflicts = append(conflicts, conflict)
	}
	return conflicts, rows.Err()
}

func (tx *Tx) InsertConflict(ctx context.Context, namespaceID, typ, detailsJSON string) error {
	return insertConflict(ctx, tx.tx, tx.workspaceID, namespaceID, typ, detailsJSON)
}

// ConflictByID returns a single conflict by id (any status), used by
// `conflicts show`/`resolve` (PROD-06).
func (s *Store) ConflictByID(ctx context.Context, id string) (Conflict, error) {
	workspaceID, err := s.WorkspaceID(ctx)
	if err != nil {
		return Conflict{}, err
	}
	var c Conflict
	err = s.db.QueryRowContext(ctx, `
SELECT id, COALESCE(namespace_id, ''), type, status, details_json
FROM conflicts
WHERE workspace_id = ? AND id = ?;
`, workspaceID, id).Scan(&c.ID, &c.NamespaceID, &c.Type, &c.Status, &c.DetailsJSON)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Conflict{}, fmt.Errorf("unknown conflict %q", id)
		}
		return Conflict{}, fmt.Errorf("read conflict: %w", err)
	}
	return c, nil
}

// ResolveConflict marks a conflict resolved and records the chosen resolution
// (PROD-06). The resolution_json captures the user's keep-local/keep-remote/
// keep-both decision for audit and cross-device sync.
func (s *Store) ResolveConflict(ctx context.Context, id, resolutionJSON string) error {
	workspaceID, err := s.WorkspaceID(ctx)
	if err != nil {
		return err
	}
	now := timestampNow()
	res, err := s.db.ExecContext(ctx, `
UPDATE conflicts SET status = 'resolved', resolution_json = ?, updated_at = ? WHERE id = ? AND workspace_id = ? AND status = 'open';
`, nullEmpty(resolutionJSON), now, id, workspaceID)
	if err != nil {
		return fmt.Errorf("resolve conflict: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("resolve conflict rows: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("conflict %q not found or already resolved", id)
	}
	return nil
}

// ResolveConflictByFingerprint marks the open conflict matching the stable
// (workspace_id, type, details_json) fingerprint resolved (PROD-06). Used by
// the conflict.resolved event apply handler so cross-device convergence does
// not depend on per-device conflict IDs. P5-SYNC-02: namespace_id is a
// locally-minted prj_<uuid> that differs per device, so it must NOT be part of
// the match — details_json already embeds the stable path and event-coordinate
// winner/loser, making (type, details_json) globally unique. It is idempotent:
// a duplicate event for an already-resolved (or absent) row affects zero rows
// and returns nil.
func (tx *Tx) ResolveConflictByFingerprint(ctx context.Context, typ, detailsJSON, resolutionJSON string) error {
	now := timestampNow()
	_, err := tx.tx.ExecContext(ctx, `
UPDATE conflicts SET status = 'resolved', resolution_json = ?, updated_at = ?
WHERE workspace_id = ? AND type = ? AND details_json = ? AND status = 'open';
`, nullEmpty(resolutionJSON), now, tx.workspaceID, typ, detailsJSON)
	if err != nil {
		return fmt.Errorf("resolve conflict by fingerprint: %w", err)
	}
	return nil
}

func insertConflict(ctx context.Context, exec sqlExecutor, workspaceID, namespaceID, typ, detailsJSON string) error {
	var existingID string
	err := exec.QueryRowContext(ctx, `
SELECT id
FROM conflicts
WHERE workspace_id = ?
  AND COALESCE(namespace_id, '') = COALESCE(?, '')
  AND type = ?
  AND status = 'open'
  AND details_json = ?
LIMIT 1;
`, workspaceID, nullEmpty(namespaceID), typ, detailsJSON).Scan(&existingID)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read existing conflict: %w", err)
	}
	conflictID, err := id.New("cnf")
	if err != nil {
		return err
	}
	now := timestampNow()
	_, err = exec.ExecContext(ctx, `
INSERT INTO conflicts (id, workspace_id, namespace_id, type, details_json, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?);
`, conflictID, workspaceID, nullEmpty(namespaceID), typ, detailsJSON, now, now)
	if err != nil {
		return fmt.Errorf("insert conflict: %w", err)
	}
	return nil
}

func (s *Store) InsertEvent(ctx context.Context, event Event) error {
	workspaceID, err := s.WorkspaceID(ctx)
	if err != nil {
		return err
	}
	_, err = insertEvent(ctx, s.db, workspaceID, normalizeEvent(event))
	return err
}

// EnsureRemoteDevice creates a placeholder device row for a remote device if it
// does not exist, so events from that device can satisfy the events FK
// constraint. The placeholder has trust_state='pending' and no keys; it is
// enriched via `devices enroll` before its events are signature-verified.
func (s *Store) EnsureRemoteDevice(ctx context.Context, deviceID string) error {
	if deviceID == "" {
		return fmt.Errorf("device id must not be empty")
	}
	now := timestampNow()
	_, err := s.db.ExecContext(ctx, `
INSERT OR IGNORE INTO devices (id, name, os, arch, trust_state, created_at, updated_at)
VALUES (?, ?, 'unknown', 'unknown', 'pending', ?, ?);
`, deviceID, "remote-"+deviceID[:min(len(deviceID), 8)], now, now)
	if err != nil {
		return fmt.Errorf("ensure remote device: %w", err)
	}
	return nil
}

// EnsureRemoteDeviceTx is the transaction-scoped version of EnsureRemoteDevice.
func (tx *Tx) EnsureRemoteDeviceTx(ctx context.Context, deviceID string) error {
	if deviceID == "" {
		return fmt.Errorf("device id must not be empty")
	}
	now := timestampNow()
	_, err := tx.tx.ExecContext(ctx, `
INSERT OR IGNORE INTO devices (id, name, os, arch, trust_state, created_at, updated_at)
VALUES (?, ?, 'unknown', 'unknown', 'pending', ?, ?);
`, deviceID, "remote", now, now)
	if err != nil {
		return fmt.Errorf("ensure remote device: %w", err)
	}
	return nil
}

func (s *Store) InsertLocalEvent(ctx context.Context, event Event) (Event, error) {
	if event.ID == "" {
		var err error
		event.ID, err = id.New("evt")
		if err != nil {
			return Event{}, err
		}
	}
	if event.CreatedAt == "" {
		event.CreatedAt = timestampNow()
	}
	if event.ContentHash == "" {
		event.ContentHash = ContentHash(event.PayloadJSON)
	}
	err := s.WithTx(ctx, func(tx *Tx) error {
		device, err := currentDevice(ctx, tx.tx)
		if err != nil {
			return err
		}
		hlc, seq, err := tx.nextLocalEventStamp(ctx, device.ID)
		if err != nil {
			return err
		}
		event.DeviceID = device.ID
		event.WorkspaceID = tx.workspaceID
		event.HLC = hlc
		event.Seq = seq
		if event.PrevEventHash == "" {
			prevHash, ok, err := previousEventContentHash(ctx, tx.tx, event)
			if err != nil {
				return err
			}
			if ok {
				event.PrevEventHash = prevHash
			}
		}
		if err := tx.ensureLocalEventSignature(ctx, s.keyDir, &event); err != nil {
			return err
		}
		inserted, err := tx.InsertEvent(ctx, event)
		if err != nil {
			return err
		}
		if !inserted {
			return fmt.Errorf("%w: %s", ErrDivergentEvent, event.ID)
		}
		return nil
	})
	if err != nil {
		return Event{}, err
	}
	return event, nil
}

func (tx *Tx) InsertEvent(ctx context.Context, event Event) (bool, error) {
	return insertEvent(ctx, tx.tx, tx.workspaceID, normalizeEvent(event))
}

func (tx *Tx) ensureLocalEventSignature(ctx context.Context, keyDir string, event *Event) error {
	if event.DeviceSig != "" {
		return nil
	}
	signing, _, err := devicekeys.NewHybridStore(keyDir, platform.Detect().Keychain).EnsureSigning(ctx, event.DeviceID)
	if err != nil {
		return fmt.Errorf("ensure local event signing identity: %w", err)
	}
	if err := tx.setDeviceSigningPublicKey(ctx, event.DeviceID, signing.Public); err != nil {
		return err
	}
	signature, err := devicekeys.Sign(signing.Private, eventSignatureDomain, EventSignaturePayload(*event))
	if err != nil {
		return fmt.Errorf("sign event: %w", err)
	}
	event.DeviceSig = signature
	return nil
}

func (tx *Tx) setDeviceSigningPublicKey(ctx context.Context, deviceID, publicKey string) error {
	now := timestampNow()
	result, err := tx.tx.ExecContext(ctx, `
UPDATE devices
SET signing_public_key = ?, updated_at = ?
WHERE id = ? AND (signing_public_key IS NULL OR signing_public_key = ?);
`, publicKey, now, deviceID, publicKey)
	if err != nil {
		return fmt.Errorf("set device signing public key: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read device signing public key update count: %w", err)
	}
	if updated == 0 {
		return fmt.Errorf("device signing public key mismatch")
	}
	return nil
}

func (tx *Tx) nextLocalEventStamp(ctx context.Context, deviceID string) (int64, int64, error) {
	var lastHLC, nextSeq int64
	err := tx.tx.QueryRowContext(ctx, `
SELECT last_hlc, next_seq
FROM device_sync_state
WHERE device_id = ?;
`, deviceID).Scan(&lastHLC, &nextSeq)
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.tx.QueryRowContext(ctx, `
SELECT COALESCE(MAX(hlc), 0), COALESCE(MAX(seq), 0) + 1
FROM events
WHERE device_id = ?;
`, deviceID).Scan(&lastHLC, &nextSeq); err != nil {
			return 0, 0, fmt.Errorf("seed local event clock: %w", err)
		}
	} else if err != nil {
		return 0, 0, fmt.Errorf("read local event clock: %w", err)
	}
	if nextSeq < 1 {
		nextSeq = 1
	}
	hlc := advanceHLC(lastHLC, time.Now().UTC())
	now := timestampNow()
	_, err = tx.tx.ExecContext(ctx, `
INSERT INTO device_sync_state (device_id, last_hlc, next_seq, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(device_id) DO UPDATE SET
  last_hlc = excluded.last_hlc,
  next_seq = excluded.next_seq,
  updated_at = excluded.updated_at;
`, deviceID, hlc, nextSeq+1, now)
	if err != nil {
		return 0, 0, fmt.Errorf("persist local event clock: %w", err)
	}
	return hlc, nextSeq, nil
}

func advanceHLC(last int64, now time.Time) int64 {
	nowPhysical := now.UnixMilli()
	lastPhysical, lastLogical := unpackHLC(last)
	switch {
	case nowPhysical > lastPhysical:
		return packHLC(nowPhysical, 0)
	case lastLogical < hlcLogicalMask:
		return packHLC(lastPhysical, lastLogical+1)
	default:
		return packHLC(lastPhysical+1, 0)
	}
}

func packHLC(physical, logical int64) int64 {
	return (physical << hlcLogicalBits) | logical
}

func unpackHLC(value int64) (physical int64, logical int64) {
	return value >> hlcLogicalBits, value & hlcLogicalMask
}

func insertEvent(ctx context.Context, exec sqlExecutor, workspaceID string, event Event) (bool, error) {
	if event.ID == "" {
		var err error
		event.ID, err = id.New("evt")
		if err != nil {
			return false, err
		}
	}
	if event.WorkspaceID == "" {
		event.WorkspaceID = workspaceID
	}
	if event.CreatedAt == "" {
		event.CreatedAt = timestampNow()
	}
	if event.ContentHash == "" {
		event.ContentHash = ContentHash(event.PayloadJSON)
	}
	expectedHash := ContentHash(event.PayloadJSON)
	if event.ContentHash != expectedHash {
		return false, fmt.Errorf("event %s content hash mismatch: got %s want %s", event.ID, event.ContentHash, expectedHash)
	}
	if err := validatePrevEventHash(ctx, exec, event); err != nil {
		return false, err
	}
	if err := verifyEventSignature(ctx, exec, event); err != nil {
		return false, err
	}
	result, err := exec.ExecContext(ctx, `
INSERT OR IGNORE INTO events (id, workspace_id, device_id, seq, hlc, type, payload_json, content_hash, device_sig, prev_event_hash, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);
`, event.ID, event.WorkspaceID, event.DeviceID, nullZero(event.Seq), event.HLC, event.Type, event.PayloadJSON, event.ContentHash, nullEmpty(event.DeviceSig), nullEmpty(event.PrevEventHash), event.CreatedAt)
	if err != nil {
		return false, fmt.Errorf("insert event: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read inserted event count: %w", err)
	}
	if inserted > 0 {
		return true, nil
	}
	var existing Event
	row := exec.QueryRowContext(ctx, `
SELECT id, workspace_id, device_id, COALESCE(seq, 0), hlc, type, payload_json, content_hash, COALESCE(device_sig, ''), COALESCE(prev_event_hash, ''), created_at
FROM events
WHERE id = ?;
`, event.ID)
	if err := row.Scan(&existing.ID, &existing.WorkspaceID, &existing.DeviceID, &existing.Seq, &existing.HLC, &existing.Type, &existing.PayloadJSON, &existing.ContentHash, &existing.DeviceSig, &existing.PrevEventHash, &existing.CreatedAt); err != nil {
		return false, fmt.Errorf("read existing event %s: %w", event.ID, err)
	}
	if !sameImmutableEvent(existing, event) {
		return false, fmt.Errorf("%w: %s", ErrDivergentEvent, event.ID)
	}
	return false, nil
}

func validatePrevEventHash(ctx context.Context, exec sqlExecutor, event Event) error {
	if event.PrevEventHash == "" {
		return nil
	}
	previousHash, ok, err := previousEventContentHash(ctx, exec, event)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: event %s references %s but no previous event exists for device %s", ErrEventHashChain, event.ID, event.PrevEventHash, event.DeviceID)
	}
	if previousHash != event.PrevEventHash {
		return fmt.Errorf("%w: event %s prev_event_hash mismatch: got %s want %s", ErrEventHashChain, event.ID, event.PrevEventHash, previousHash)
	}
	return nil
}

func previousEventContentHash(ctx context.Context, exec sqlExecutor, event Event) (string, bool, error) {
	var hash string
	var err error
	if event.Seq > 1 {
		err = exec.QueryRowContext(ctx, `
SELECT content_hash
FROM events
WHERE workspace_id = ? AND device_id = ? AND seq = ?
LIMIT 1;
`, event.WorkspaceID, event.DeviceID, event.Seq-1).Scan(&hash)
	} else if event.Seq == 1 {
		return "", false, nil
	} else {
		err = exec.QueryRowContext(ctx, `
SELECT content_hash
FROM events
WHERE workspace_id = ?
  AND device_id = ?
  AND (hlc < ? OR (hlc = ? AND id < ?))
ORDER BY hlc DESC, id DESC
LIMIT 1;
`, event.WorkspaceID, event.DeviceID, event.HLC, event.HLC, event.ID).Scan(&hash)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read previous event hash: %w", err)
	}
	return hash, true, nil
}

const eventSignatureDomain = "devstrap:event:v1"

type eventSignaturePayload struct {
	ContentHash   string `json:"content_hash"`
	HLC           int64  `json:"hlc"`
	ID            string `json:"id"`
	PayloadJSON   string `json:"payload_json"`
	PrevEventHash string `json:"prev_event_hash"`
	Type          string `json:"type"`
}

func EventSignaturePayload(event Event) []byte {
	raw, err := json.Marshal(eventSignaturePayload{
		ContentHash:   event.ContentHash,
		HLC:           event.HLC,
		ID:            event.ID,
		PayloadJSON:   event.PayloadJSON,
		PrevEventHash: event.PrevEventHash,
		Type:          event.Type,
	})
	if err != nil {
		panic(err)
	}
	return raw
}

func verifyEventSignature(ctx context.Context, exec sqlExecutor, event Event) error {
	var signingPublicKey string
	var trustState string
	err := exec.QueryRowContext(ctx, `
SELECT COALESCE(signing_public_key, ''), trust_state
FROM devices
WHERE id = ?;
`, event.DeviceID).Scan(&signingPublicKey, &trustState)
	// HUB-03: once the workspace has any enrolled (approved, non-local) device,
	// event verification fails CLOSED for ALL event types from non-local
	// devices. Before enrollment, only destructive event types require
	// verification (the bootstrap window). The local device is always exempt
	// from the signing-key requirement (pre-enrollment grace).
	enrolled, enrollErr := hasEnrolledDevices(ctx, exec)
	if enrollErr != nil {
		return fmt.Errorf("check enrolled devices: %w", enrollErr)
	}
	if errors.Is(err, sql.ErrNoRows) {
		// Unknown device. Once enrolled, reject everything from unknown devices.
		// Before enrollment, reject only destructive events.
		if mustVerifyEvent(event.Type) || enrolled {
			return fmt.Errorf("event %s of type %s requires a signature from a known approved device", event.ID, event.Type)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read device signing public key: %w", err)
	}
	isLocal := trustState == "local"
	if signingPublicKey == "" {
		// A device with no signing key may not inject events that require
		// verification, EXCEPT the local device (which may not have signing
		// set up yet). Before enrollment, non-destructive events from any
		// known device are accepted.
		if !isLocal && (mustVerifyEvent(event.Type) || enrolled) {
			return fmt.Errorf("event %s of type %s requires a signature from a device with a signing key", event.ID, event.Type)
		}
		return nil
	}
	if event.DeviceSig == "" {
		if mustVerifyEvent(event.Type) {
			return fmt.Errorf("event %s of type %s requires a device signature", event.ID, event.Type)
		}
		return fmt.Errorf("event %s missing device signature", event.ID)
	}
	// HUB-03: for must-verify events, require the device to be approved (the
	// local device is exempt). For non-must-verify events once enrolled,
	// require non-local devices to be approved too (fail-closed).
	if !isLocal && trustState != "approved" && (mustVerifyEvent(event.Type) || enrolled) {
		return fmt.Errorf("event %s of type %s requires a signature from an approved device (current: %s)", event.ID, event.Type, trustState)
	}
	if err := devicekeys.Verify(signingPublicKey, event.DeviceSig, eventSignatureDomain, EventSignaturePayload(event)); err != nil {
		return fmt.Errorf("event %s device signature invalid: %w", event.ID, err)
	}
	return nil
}

// hasEnrolledDevices reports whether the workspace has any approved, non-local
// device (HUB-03). Once true, event verification fails closed for all non-local
// event types.
func hasEnrolledDevices(ctx context.Context, exec sqlExecutor) (bool, error) {
	var count int
	if err := exec.QueryRowContext(ctx, `
SELECT COUNT(*) FROM devices WHERE trust_state = 'approved';
`).Scan(&count); err != nil {
		// The devices table may not exist yet during early bootstrap (before
		// migration 00004); treat only that specific error as "not enrolled".
		// All other errors (locked DB, corruption, etc.) must propagate so
		// HUB-03 fail-closed verification is not silently downgraded.
		if strings.Contains(err.Error(), "no such table") {
			return false, nil
		}
		return false, fmt.Errorf("check enrolled devices: %w", err)
	}
	return count > 0, nil
}

// mustVerifyEvent reports whether an event type is destructive or
// trust-affecting and therefore requires a valid signature from a known,
// approved device (SECU-03). Unknown devices and devices with no signing key
// must not be able to inject these events.
func mustVerifyEvent(eventType string) bool {
	switch eventType {
	case "project.deleted", "project.renamed":
		return true
	default:
		return false
	}
}

func ContentHash(payloadJSON string) string {
	sum := sha256.Sum256([]byte(payloadJSON))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func normalizeEvent(event Event) Event {
	if event.CreatedAt == "" {
		event.CreatedAt = timestampNow()
	}
	if event.ContentHash == "" {
		event.ContentHash = ContentHash(event.PayloadJSON)
	}
	return event
}

func sameImmutableEvent(a, b Event) bool {
	return a.WorkspaceID == b.WorkspaceID &&
		a.DeviceID == b.DeviceID &&
		a.Seq == b.Seq &&
		a.HLC == b.HLC &&
		a.Type == b.Type &&
		a.PayloadJSON == b.PayloadJSON &&
		a.ContentHash == b.ContentHash &&
		a.DeviceSig == b.DeviceSig &&
		a.PrevEventHash == b.PrevEventHash
}

func (s *Store) PendingEvents(ctx context.Context) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, workspace_id, device_id, COALESCE(seq, 0), hlc, type, payload_json, content_hash, COALESCE(device_sig, ''), COALESCE(prev_event_hash, ''), created_at
FROM events
ORDER BY hlc ASC, device_id ASC, id ASC;
`)
	if err != nil {
		return nil, fmt.Errorf("list pending events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var events []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.WorkspaceID, &e.DeviceID, &e.Seq, &e.HLC, &e.Type, &e.PayloadJSON, &e.ContentHash, &e.DeviceSig, &e.PrevEventHash, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

// LocalPendingEvents returns events originated by the local device with HLC
// strictly greater than afterHLC (SYNC-04). It bounds the push side of sync so
// a cycle re-uploads only new local-origin events, not the entire event log
// (including remote-origin events the hub already holds from their origin
// device). The push cursor is stored per hub as a "push:<hubID>" row in
// hub_cursors.
func (s *Store) LocalPendingEvents(ctx context.Context, afterHLC int64) ([]Event, error) {
	device, err := s.CurrentDevice(ctx)
	if err != nil {
		return nil, fmt.Errorf("read current device for local pending events: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, workspace_id, device_id, COALESCE(seq, 0), hlc, type, payload_json, content_hash, COALESCE(device_sig, ''), COALESCE(prev_event_hash, ''), created_at
FROM events
WHERE device_id = ? AND hlc > ?
ORDER BY hlc ASC, id ASC;
`, device.ID, afterHLC)
	if err != nil {
		return nil, fmt.Errorf("list local pending events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var events []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.WorkspaceID, &e.DeviceID, &e.Seq, &e.HLC, &e.Type, &e.PayloadJSON, &e.ContentHash, &e.DeviceSig, &e.PrevEventHash, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

// EventByID returns a single event by id. Used by conflict resolution
// (P5-SYNC-04) to recover the full payload of a losing variant so a chosen
// remote can be re-asserted with a fresh dominating event.
func (s *Store) EventByID(ctx context.Context, id string) (Event, error) {
	var e Event
	err := s.db.QueryRowContext(ctx, `
SELECT id, workspace_id, device_id, COALESCE(seq, 0), hlc, type, payload_json, content_hash, COALESCE(device_sig, ''), COALESCE(prev_event_hash, ''), created_at
FROM events WHERE id = ?;
`, id).Scan(&e.ID, &e.WorkspaceID, &e.DeviceID, &e.Seq, &e.HLC, &e.Type, &e.PayloadJSON, &e.ContentHash, &e.DeviceSig, &e.PrevEventHash, &e.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, fmt.Errorf("unknown event %q", id)
	}
	if err != nil {
		return Event{}, fmt.Errorf("read event: %w", err)
	}
	return e, nil
}

// HubCursor returns the last HLC applied from the given hub source (EAGER-02).
// Returns 0 when no cursor exists yet (a fresh device pulls from the beginning).
func (s *Store) HubCursor(ctx context.Context, hubID string) (int64, error) {
	workspaceID, err := s.WorkspaceID(ctx)
	if err != nil {
		return 0, err
	}
	var last int64
	err = s.db.QueryRowContext(ctx, `
SELECT last_hlc_applied FROM hub_cursors WHERE workspace_id = ? AND hub_id = ?;
`, workspaceID, hubID).Scan(&last)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read hub cursor: %w", err)
	}
	return last, nil
}

// AdvanceHubCursor records that all events up to hlc have been applied from the
// given hub source (EAGER-02). It only moves the cursor forward: a smaller hlc
// than the stored value is ignored so a re-pull of stale events cannot regress
// the cursor.
func (s *Store) AdvanceHubCursor(ctx context.Context, hubID string, hlc int64) error {
	workspaceID, err := s.WorkspaceID(ctx)
	if err != nil {
		return err
	}
	now := timestampNow()
	_, err = s.db.ExecContext(ctx, `
INSERT INTO hub_cursors (workspace_id, hub_id, last_hlc_applied, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(workspace_id, hub_id) DO UPDATE SET
  last_hlc_applied = MAX(excluded.last_hlc_applied, hub_cursors.last_hlc_applied),
  updated_at = excluded.updated_at
WHERE excluded.last_hlc_applied > hub_cursors.last_hlc_applied;
`, workspaceID, hubID, hlc, now)
	if err != nil {
		return fmt.Errorf("advance hub cursor: %w", err)
	}
	return nil
}

// SkeletonProjects returns all active projects whose local materialization state
// is "skeleton" or "failed" — the set the eager materialization pass (EAGER-01)
// must touch. A re-run only revisits projects that still need work, making the
// pass idempotent and resumable (EAGER-04).
func (s *Store) SkeletonProjects(ctx context.Context) ([]ProjectStatus, error) {
	all, err := s.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	var out []ProjectStatus
	for _, p := range all {
		if p.MaterializationState == "" || p.MaterializationState == "skeleton" || p.MaterializationState == "failed" {
			out = append(out, p)
		}
	}
	return out, nil
}

func (s *Store) InsertWorktree(ctx context.Context, wt Worktree) (Worktree, error) {
	if wt.ID == "" {
		var err error
		wt.ID, err = id.New("wt")
		if err != nil {
			return Worktree{}, err
		}
	}
	if wt.Status == "" {
		wt.Status = "active"
	}
	if wt.DirtyState == "" {
		wt.DirtyState = "clean"
	}
	now := timestampNow()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO worktrees (id, namespace_id, device_id, path, branch, base_ref, base_sha, created_by, status, dirty_state, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);
`, wt.ID, wt.NamespaceID, wt.DeviceID, wt.Path, wt.Branch, wt.BaseRef, wt.BaseSHA, wt.CreatedBy, wt.Status, wt.DirtyState, now, now)
	if err != nil {
		return Worktree{}, fmt.Errorf("insert worktree: %w", err)
	}
	return wt, nil
}

func (s *Store) ListWorktrees(ctx context.Context) ([]Worktree, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, namespace_id, device_id, path, branch, base_ref, base_sha, created_by, status, dirty_state
FROM worktrees
WHERE status = 'active'
ORDER BY created_at DESC, id DESC;
`)
	if err != nil {
		return nil, fmt.Errorf("list worktrees: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Worktree
	for rows.Next() {
		var wt Worktree
		if err := rows.Scan(&wt.ID, &wt.NamespaceID, &wt.DeviceID, &wt.Path, &wt.Branch, &wt.BaseRef, &wt.BaseSHA, &wt.CreatedBy, &wt.Status, &wt.DirtyState); err != nil {
			return nil, fmt.Errorf("scan worktree: %w", err)
		}
		out = append(out, wt)
	}
	return out, rows.Err()
}

func (s *Store) WorktreeByID(ctx context.Context, id string) (Worktree, error) {
	var wt Worktree
	err := s.db.QueryRowContext(ctx, `
SELECT id, namespace_id, device_id, path, branch, base_ref, base_sha, created_by, status, dirty_state
FROM worktrees
WHERE id = ? AND status = 'active';
`, id).Scan(&wt.ID, &wt.NamespaceID, &wt.DeviceID, &wt.Path, &wt.Branch, &wt.BaseRef, &wt.BaseSHA, &wt.CreatedBy, &wt.Status, &wt.DirtyState)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Worktree{}, fmt.Errorf("unknown active worktree %q", id)
		}
		return Worktree{}, fmt.Errorf("read worktree: %w", err)
	}
	return wt, nil
}

func (s *Store) MarkWorktreeRemoved(ctx context.Context, id string) error {
	now := timestampNow()
	_, err := s.db.ExecContext(ctx, `
UPDATE worktrees SET status = 'removed', updated_at = ? WHERE id = ?;
`, now, id)
	if err != nil {
		return fmt.Errorf("mark worktree removed: %w", err)
	}
	return nil
}

func (s *Store) InsertAgentRun(ctx context.Context, run AgentRun) (AgentRun, error) {
	if run.ID == "" {
		var err error
		run.ID, err = id.New("arun")
		if err != nil {
			return AgentRun{}, err
		}
	}
	if run.Status == "" {
		run.Status = "running"
	}
	now := timestampNow()
	_, err := s.db.ExecContext(ctx, `
INSERT INTO agent_runs (id, namespace_id, worktree_id, engine, task, policy_id, status, base_ref, base_sha, branch, log_path, diff_summary, test_summary, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);
`, run.ID, run.NamespaceID, nullEmpty(run.WorktreeID), run.Engine, run.Task, nullEmpty(run.PolicyID), run.Status, nullEmpty(run.BaseRef), nullEmpty(run.BaseSHA), nullEmpty(run.Branch), nullEmpty(run.LogPath), nullEmpty(run.DiffSummary), nullEmpty(run.TestSummary), now, now)
	if err != nil {
		return AgentRun{}, fmt.Errorf("insert agent run: %w", err)
	}
	return run, nil
}

func (s *Store) UpdateAgentRunResult(ctx context.Context, id, status, diffSummary, testSummary string) error {
	now := timestampNow()
	_, err := s.db.ExecContext(ctx, `
UPDATE agent_runs
SET status = ?, diff_summary = ?, test_summary = ?, updated_at = ?
WHERE id = ?;
`, status, nullEmpty(diffSummary), nullEmpty(testSummary), now, id)
	if err != nil {
		return fmt.Errorf("update agent run result: %w", err)
	}
	return nil
}

func (s *Store) AgentRunByID(ctx context.Context, id string) (AgentRun, error) {
	var run AgentRun
	err := s.db.QueryRowContext(ctx, `
SELECT id, namespace_id, COALESCE(worktree_id, ''), engine, task, COALESCE(policy_id, ''), status,
       COALESCE(base_ref, ''), COALESCE(base_sha, ''), COALESCE(branch, ''), COALESCE(log_path, ''),
       COALESCE(diff_summary, ''), COALESCE(test_summary, '')
FROM agent_runs
WHERE id = ?;
`, id).Scan(&run.ID, &run.NamespaceID, &run.WorktreeID, &run.Engine, &run.Task, &run.PolicyID, &run.Status, &run.BaseRef, &run.BaseSHA, &run.Branch, &run.LogPath, &run.DiffSummary, &run.TestSummary)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AgentRun{}, fmt.Errorf("unknown agent run %q", id)
		}
		return AgentRun{}, fmt.Errorf("read agent run: %w", err)
	}
	return run, nil
}

func (s *Store) ListAgentRuns(ctx context.Context) ([]AgentRun, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, namespace_id, COALESCE(worktree_id, ''), engine, task, COALESCE(policy_id, ''), status,
       COALESCE(base_ref, ''), COALESCE(base_sha, ''), COALESCE(branch, ''), COALESCE(log_path, ''),
       COALESCE(diff_summary, ''), COALESCE(test_summary, '')
FROM agent_runs
ORDER BY created_at DESC, id DESC;
`)
	if err != nil {
		return nil, fmt.Errorf("list agent runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var runs []AgentRun
	for rows.Next() {
		var run AgentRun
		if err := rows.Scan(&run.ID, &run.NamespaceID, &run.WorktreeID, &run.Engine, &run.Task, &run.PolicyID, &run.Status, &run.BaseRef, &run.BaseSHA, &run.Branch, &run.LogPath, &run.DiffSummary, &run.TestSummary); err != nil {
			return nil, fmt.Errorf("scan agent run: %w", err)
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

func filepathBaseSlash(path string) string {
	path = strings.Trim(path, "/")
	if path == "" {
		return ""
	}
	parts := strings.Split(path, "/")
	return parts[len(parts)-1]
}

func nullEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullZero(value int64) any {
	if value == 0 {
		return nil
	}
	return value
}
