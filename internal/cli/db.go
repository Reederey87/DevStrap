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
			store, err := opts.openState()
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
			store, err := opts.openState()
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

	cmd.AddCommand(&cobra.Command{
		Use:   "backup [path]",
		Short: "Write a consistent state database backup",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out, err := filepath.Abs(filepath.Clean(args[0]))
			if err != nil {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("resolve backup path: %w", err)}
			}
			store, err := opts.openState()
			if err != nil {
				return err
			}
			defer closeStore(store)
			if err := store.Backup(cmd.Context(), out); err != nil {
				return err
			}
			_, err = fmt.Fprintf(stdout, "backup written: %s\n", out)
			return err
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "down",
		Short: "Roll back one state database migration",
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := opts.openState()
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
