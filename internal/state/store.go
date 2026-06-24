package state

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"time"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

type Store struct {
	db *sql.DB
}

type Summary struct {
	WorkspaceName string `json:"workspace_name"`
	RootPath      string `json:"root_path"`
	ProjectCount  int    `json:"project_count"`
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite state: %w", err)
	}
	if _, err := db.Exec("PRAGMA foreign_keys=ON; PRAGMA journal_mode=WAL;"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("configure sqlite state: %w", err)
	}
	return &Store{db: db}, nil
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

func (s *Store) EnsureWorkspace(ctx context.Context, name, rootPath string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
INSERT INTO workspaces (id, name, root_path, created_at, updated_at)
VALUES ('ws_local', ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
  name = excluded.name,
  root_path = excluded.root_path,
  updated_at = excluded.updated_at;
`, name, rootPath, now, now)
	if err != nil {
		return fmt.Errorf("ensure workspace: %w", err)
	}
	return nil
}

func (s *Store) Summary(ctx context.Context) (Summary, error) {
	var summary Summary
	row := s.db.QueryRowContext(ctx, `
SELECT w.name, w.root_path, COUNT(n.id)
FROM workspaces w
LEFT JOIN namespace_entries n ON n.workspace_id = w.id
WHERE w.id = 'ws_local'
GROUP BY w.id, w.name, w.root_path;
`)
	if err := row.Scan(&summary.WorkspaceName, &summary.RootPath, &summary.ProjectCount); err != nil {
		if err == sql.ErrNoRows {
			return Summary{}, fmt.Errorf("workspace is not initialized; run devstrap init")
		}
		return Summary{}, fmt.Errorf("read workspace summary: %w", err)
	}
	return summary, nil
}
