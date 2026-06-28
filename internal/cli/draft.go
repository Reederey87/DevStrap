package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"

	"github.com/Reederey87/DevStrap/internal/draftbundle"
	"github.com/Reederey87/DevStrap/internal/ignore"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
	"github.com/spf13/cobra"
)

func newDraftCommand(stdout io.Writer, opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "draft",
		Short: "Manage non-git draft project content sync",
	}
	cmd.AddCommand(newDraftSnapshotCommand(stdout, opts))
	return cmd
}

func newDraftSnapshotCommand(stdout io.Writer, opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "snapshot",
		Short: "Draft snapshot operations",
	}
	cmd.AddCommand(newDraftSnapshotCreateCommand(stdout, opts))
	return cmd
}

func newDraftSnapshotCreateCommand(stdout io.Writer, opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create <path>",
		Short: "Pack a non-git project into an encrypted draft bundle and emit a snapshot event",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			project, err := store.ProjectByPath(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if project.Type == "git_repo" {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("%s is git_repo; use git for repo content", project.Path)}
			}
			localPath := project.LocalPath
			if localPath == "" {
				localPath = filepath.Join(opts.paths().Root, filepath.FromSlash(project.Path))
			}
			if err := store.EnsureDraftProject(cmd.Context(), project.ID); err != nil {
				return err
			}
			maxBytes, maxFiles, err := store.DraftProjectLimits(cmd.Context(), project.ID)
			if err != nil {
				return err
			}
			recipients, err := store.ApprovedRecipients(cmd.Context())
			if err != nil {
				return err
			}
			// DRAFT-03: one compiled ignore policy is the source of truth for
			// what is bundled, pruned, and excluded.
			matcher, err := ignore.CompileFromDir(localPath, true)
			if err != nil {
				return err
			}
			snap, err := draftbundle.Pack(localPath, matcher, draftbundle.Limits{
				MaxBytes: maxBytes,
				MaxFiles: maxFiles,
			}, recipients)
			if err != nil {
				return appError{code: exitInvalidConfig, err: err}
			}
			if err := writeEnvBlob(opts.paths(), snap.BlobRef, snap.Ciphertext); err != nil {
				return err
			}
			payload := dssync.DraftSnapshotPayload{
				Path:      project.Path,
				BlobRef:   snap.BlobRef,
				ByteSize:  snap.ByteSize,
				FileCount: snap.FileCount,
			}
			raw, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			if _, err := store.InsertLocalEvent(cmd.Context(), dssync.NewDraftSnapshotEvent(dssync.EventDraftSnapshotCreated, string(raw))); err != nil {
				return err
			}
			_, err = fmt.Fprintf(stdout, "Created draft snapshot for %s: %s (%d files, %d bytes) for %d recipient device(s)\n",
				project.Path, snap.BlobRef, snap.FileCount, snap.ByteSize, len(recipients))
			return err
		},
	}
	return cmd
}
