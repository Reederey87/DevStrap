package cli

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/spf13/cobra"
)

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
			_, err = fmt.Fprintf(stdout, "schema version: %d\n", version)
			return err
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
			_, err = fmt.Fprintf(stdout, "schema version: %d\nsqlite quick_check: %s\nsqlite foreign_key_check: %s\n", version, check, fkCheck)
			return err
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
			_, err = fmt.Fprintf(stdout, "backup written: %s\n", out)
			return err
		},
	}
	backupCmd.Flags().BoolVar(&fullBackup, "full", false, "write a tar archive with the database, encrypted blobs, and key material")
	cmd.AddCommand(backupCmd)

	var restoreForce, restoreAllowLegacy bool
	restoreCmd := &cobra.Command{
		Use:   "restore [archive.tar]",
		Short: "Restore a full backup archive into the state directory",
		Long: "Restore a `devstrap db backup --full` archive: extract the database,\n" +
			"encrypted blobs, and key material back into the state directory. Refuses\n" +
			"to overwrite a non-empty state directory unless --force is given.",
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			in, err := filepath.Abs(filepath.Clean(args[0]))
			if err != nil {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("resolve archive path: %w", err)}
			}
			return runRestore(cmd.Context(), opts, in, restoreForce, restoreAllowLegacy, stdout)
		},
	}
	restoreCmd.Flags().BoolVar(&restoreForce, "force", false, "overwrite a non-empty state directory")
	restoreCmd.Flags().BoolVar(&restoreAllowLegacy, "allow-legacy", false, "restore a pre-P7 archive without manifest integrity verification")
	cmd.AddCommand(restoreCmd)

	cmd.AddCommand(&cobra.Command{
		Use:   "down",
		Short: "Roll back one state database migration",
		RunE: func(cmd *cobra.Command, args []string) error {
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
			_, err = fmt.Fprintf(stdout, "schema version: %d\n", version)
			return err
		},
	})

	return cmd
}
