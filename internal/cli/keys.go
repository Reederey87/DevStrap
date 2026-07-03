package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

func newKeysCommand(stdout io.Writer, opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "keys",
		Short: "Manage the workspace content key (WCK) epochs",
	}
	cmd.AddCommand(newKeysRotateCommand(stdout, opts))
	return cmd
}

// newKeysRotateCommand implements `devstrap keys rotate` (P4-SEC-07 periodic
// rotation). It calls Keyring.Rotate DIRECTLY — deliberately NOT the `devices
// revoke` path: a periodic rotation has no excluded device, so it must not
// flag secrets for source rotation, must not re-encrypt blobs, and must not
// queue hub ciphertext deletions. It bounds FORWARD exposure only (a silently
// compromised key stops reading new events once the fleet converges on the
// new epoch); it gives no retroactive protection — for a known-compromised
// device use `devices revoke`, which layers all of the above on top.
func newKeysRotateCommand(stdout io.Writer, opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "rotate",
		Short: "Mint a fresh workspace key epoch and grant it to all approved devices",
		Long: `Mint a fresh workspace content key at epoch+1 and grant it to every
approved device (one device.key.granted event per recipient; the next
'devstrap sync' publishes them, and 'sync' also does this automatically when
the active epoch is older than keys.rotate_max_age).

Periodic rotation bounds FORWARD exposure only: a silently compromised key
stops decrypting new events once the fleet converges on the new epoch. It does
not revoke anything retroactively and does not touch blobs or secrets — for a
lost or compromised device use 'devstrap devices revoke', which additionally
re-encrypts blobs and flags secrets for source rotation.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			kr := buildKeyring(opts, store)
			epoch, err := kr.CurrentEpoch(cmd.Context())
			if err != nil {
				return err
			}
			if epoch == 0 {
				return appError{code: exitInvalidConfig, err: fmt.Errorf(
					"no workspace key epoch exists yet: the key is founded on the first sync (or granted on approval) — nothing to rotate")}
			}
			newEpoch, grants, err := kr.Rotate(cmd.Context())
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(stdout, "Rotated workspace key to epoch %d; queued %d grant event(s); run 'devstrap sync' to publish\n", newEpoch, len(grants))
			return err
		},
	}
}
