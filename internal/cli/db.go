package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

type restoreRecoveryResult struct {
	Recovered  bool `json:"recovered"`
	RolledBack bool `json:"rolled_back"`
}

// P5-CLI-01 part B: plain db leaf --json shapes (not backup --full / restore).
type dbMigrateResult struct {
	Version int64 `json:"version"`
}

type dbStatusResult struct {
	Version         int64  `json:"version"`
	QuickCheck      string `json:"quick_check"`
	ForeignKeyCheck string `json:"foreign_key_check"`
}

type dbBackupResult struct {
	Path string `json:"path"`
}

type dbDownResult struct {
	Version int64 `json:"version"`
}

func newDBCommand(stdout io.Writer, opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "db",
		Short: "Manage the local state database",
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "migrate",
		Short: "Apply pending state database migrations",
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			if err := store.Migrate(); err != nil {
				return err
			}
			version, err := store.Version()
			if err != nil {
				return err
			}
			result := dbMigrateResult{Version: version}
			return opts.render(stdout, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "schema version: %d\n", version)
				return err
			}, result)
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show state database migration status",
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			version, err := store.Version()
			if err != nil {
				return err
			}
			check, err := store.QuickCheck(cmd.Context())
			if err != nil {
				return err
			}
			fkCheck, err := store.ForeignKeyCheck(cmd.Context())
			if err != nil {
				return err
			}
			result := dbStatusResult{
				Version:         version,
				QuickCheck:      check,
				ForeignKeyCheck: fkCheck,
			}
			return opts.render(stdout, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "schema version: %d\nsqlite quick_check: %s\nsqlite foreign_key_check: %s\n", version, check, fkCheck)
				return err
			}, result)
		},
	})

	var fullBackup bool
	backupCmd := &cobra.Command{
		Use:   "backup [path]",
		Short: "Write a consistent state database backup (add --full for a recoverable secrets archive)",
		Long: "Write a consistent state database backup.\n\n" +
			"By default this writes the SQLite state database only. That is NOT a\n" +
			"recoverable backup on its own: a workspace's captured secrets live in\n" +
			"age-encrypted blobs and are decryptable only with the device/workspace\n" +
			"key material. Use --full to write a single tar archive containing the\n" +
			"database, the referenced encrypted blobs, and the key material; restore\n" +
			"it with `devstrap db restore`.",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			out, err := filepath.Abs(filepath.Clean(args[0]))
			if err != nil {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("resolve backup path: %w", err)}
			}
			if fullBackup {
				unlock, err := acquireMaintenanceLock(opts.paths().Home)
				if err != nil {
					return err
				}
				defer unlock()
			}
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			if fullBackup {
				return runFullBackup(cmd.Context(), opts, store, out, stdout)
			}
			if err := store.Backup(cmd.Context(), out); err != nil {
				return err
			}
			result := dbBackupResult{Path: out}
			return opts.render(stdout, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "backup written: %s\n", out)
				return err
			}, result)
		},
	}
	backupCmd.Flags().BoolVar(&fullBackup, "full", false, "write a tar archive with the database, encrypted blobs, and key material")
	cmd.AddCommand(backupCmd)

	var restoreForce, restoreAllowLegacy, restoreRecover bool
	restoreCmd := &cobra.Command{
		Use:   "restore [archive.tar]",
		Short: "Restore a full backup archive into the state directory",
		Long: "Restore a `devstrap db backup --full` archive: extract the database,\n" +
			"encrypted blobs, and key material back into the state directory. Refuses\n" +
			"to overwrite a non-empty state directory unless --force is given.",
		Args: func(cmd *cobra.Command, args []string) error {
			if restoreRecover {
				if len(args) != 0 {
					return appError{code: exitUsage, err: fmt.Errorf("--recover does not accept an archive path")}
				}
				return nil
			}
			return usageArgs(cobra.ExactArgs(1))(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			unlock, err := acquireMaintenanceLock(opts.paths().Home)
			if err != nil {
				return err
			}
			defer unlock()
			if restoreRecover {
				if _, err := os.Stat(restoreJournalPath(opts.paths().Home)); errors.Is(err, os.ErrNotExist) {
					result := restoreRecoveryResult{}
					return opts.render(stdout, func(w io.Writer) error {
						_, err := fmt.Fprintln(w, "no interrupted restore journal found; nothing to recover")
						return err
					}, result)
				} else if err != nil {
					return fmt.Errorf("inspect restore journal: %w", err)
				}
				rolledBack, err := recoverRestoreJournal(opts.paths().Home)
				if err != nil {
					return err
				}
				result := restoreRecoveryResult{Recovered: true, RolledBack: rolledBack}
				return opts.render(stdout, func(w io.Writer) error {
					if rolledBack {
						_, err = fmt.Fprintln(w, "interrupted restore rolled back; previous state intact; re-run devstrap db restore")
					} else {
						_, err = fmt.Fprintln(w, "interrupted restore completed")
					}
					return err
				}, result)
			}
			// A plain restore auto-recovers a prior interrupted promotion before
			// validating and promoting the newly supplied archive.
			if _, err := recoverRestoreJournal(opts.paths().Home); err != nil {
				return err
			}
			in, err := filepath.Abs(filepath.Clean(args[0]))
			if err != nil {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("resolve archive path: %w", err)}
			}
			return runRestore(cmd.Context(), opts, in, restoreForce, restoreAllowLegacy, stdout)
		},
	}
	restoreCmd.Flags().BoolVar(&restoreForce, "force", false, "overwrite a non-empty state directory")
	restoreCmd.Flags().BoolVar(&restoreAllowLegacy, "allow-legacy", false, "restore a pre-P7 archive without manifest integrity verification")
	restoreCmd.Flags().BoolVar(&restoreRecover, "recover", false, "recover an interrupted journaled restore (takes no archive path)")
	cmd.AddCommand(restoreCmd)

	cmd.AddCommand(&cobra.Command{
		Use:   "down",
		Short: "Roll back one state database migration",
		RunE: func(cmd *cobra.Command, args []string) error {
			unlock, err := acquireMaintenanceLock(opts.paths().Home)
			if err != nil {
				return err
			}
			defer unlock()
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			if err := store.Down(); err != nil {
				return err
			}
			version, err := store.Version()
			if err != nil {
				return err
			}
			result := dbDownResult{Version: version}
			return opts.render(stdout, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "schema version: %d\n", version)
				return err
			}, result)
		},
	})

	return cmd
}
