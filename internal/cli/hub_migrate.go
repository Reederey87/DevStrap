package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/Reederey87/DevStrap/internal/state"
	dssync "github.com/Reederey87/DevStrap/internal/sync"
	"github.com/spf13/cobra"
)

func newHubMigrateEventsCommand(stdout io.Writer, opts *options) *cobra.Command {
	var hubFile string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "migrate-events",
		Short: "Re-key the retired HLC-keyed legacy event layout into the per-device seq layout (P4-HUB-12)",
		Long: `Re-key the retired HLC-keyed legacy event objects
(workspaces/<ws>/events/<hlc>/<device>/<seq>/<id>.json) into the current
per-device seq layout (workspaces/<ws>/eventlog/<device>/<seq>_<id>.json) and
delete the migrated legacy objects, so the dual-read Pull freezes to a cheap
empty-prefix list.

Migration is idempotent and resumable: the dual-read keeps unmigrated objects
live, each object is verified by read-back on the new key before its legacy copy
is deleted, and re-running a fully migrated hub reports 0 to migrate. It FAILS
OPEN — an object whose key cannot be parsed, whose body cannot be decoded, or
whose body (device, seq) disagree with its key is reported and KEPT, never
deleted (a parse bug must never delete an event it cannot account for).

Run migrate-events once per pre-#59 hub. The file-backed test hub never used the
legacy layout, so against --hub-file it is a no-op. Concurrent destructive hub
passes are serialized by the advisory sweep lock. --dry-run classifies the
legacy objects and reports the plan without writing anything.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			hub, _, err := hubFromOptions(cmd.Context(), opts, store, hubFile)
			if err != nil {
				return appError{code: exitInvalidConfig, err: err}
			}
			migrated, kept, err := hubMigrateEvents(cmd.Context(), store, hub, dryRun)
			if err != nil {
				return err
			}
			verb := "migrated"
			if dryRun {
				verb = "would migrate"
			}
			_, err = fmt.Fprintf(stdout, "hub migrate-events: %s %d legacy event(s); kept %d unmigratable object(s)\n", verb, migrated, kept)
			return err
		},
	}
	cmd.Flags().StringVar(&hubFile, "hub-file", "", "file-backed test hub path")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "classify the legacy objects and report the plan without writing anything")
	return cmd
}

// hubMigrateEvents re-keys the legacy event layout under the advisory sweep lock
// (P4-HUB-12). A real run acquires the sweep lock first so a concurrent gc /
// compact / migrate-events on a cooperating client does not interleave; a dry
// run writes nothing and needs no lock.
func hubMigrateEvents(ctx context.Context, store *state.Store, hub dssync.Hub, dryRun bool) (migrated, kept int, err error) {
	if !dryRun {
		release, lerr := hubSweepLock(ctx, store, hub, defaultSweepLockTTL)
		if lerr != nil {
			return 0, 0, lerr
		}
		defer release()
	}
	return hub.MigrateLegacyEvents(ctx, dryRun)
}
