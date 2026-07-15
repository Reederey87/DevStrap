package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/Reederey87/DevStrap/internal/pairing"
	"github.com/spf13/cobra"
)

// newJoinCommand implements `devstrap join <pairing-code>` (P7-PROD-01 slice 1):
// a fresh second device joins an existing workspace in ONE command. It folds
// `init --join --code` (adopt the workspace id + pin the founder), an automatic
// `hub init` when the code carries a hub URI, and generating this device's own
// pairing code for the founder to approve.
//
// Fingerprint trust: a v2 code carries the founder's fingerprint, so join
// auto-trusts it by default (no prompt) — this trusts the paste channel, it is
// NOT cryptographic authentication. Passing --fingerprint enforces the
// high-assurance out-of-band compare (constant-time; refuses on mismatch). A v1
// code (no embedded fingerprint) falls back to init --join --code's existing
// TTY-prompt / non-TTY pending behavior.
func newJoinCommand(stdout io.Writer, opts *options) *cobra.Command {
	var fingerprint string
	var workspaceName string
	cmd := &cobra.Command{
		Use:   "join <pairing-code>",
		Short: "Join an existing workspace from a founder's pairing code in one step (P7-PROD-01)",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			codeBlob := args[0]
			code, err := pairing.Decode(codeBlob)
			if err != nil {
				return appError{code: exitInvalidConfig, err: err}
			}
			// Auto-trust the embedded fingerprint unless the operator opted into
			// the high-assurance out-of-band compare via --fingerprint (which
			// runInit enforces against the carried keys and refuses on mismatch).
			autoTrust := code.HasFingerprint() && strings.TrimSpace(fingerprint) == ""
			if err := runInit(cmd, nil, stdout, opts, initParams{
				workspaceName:    workspaceName,
				join:             true,
				codeBlob:         codeBlob,
				fingerprint:      fingerprint,
				autoTrustFounder: autoTrust,
				calledFromJoin:   true,
			}); err != nil {
				return err
			}

			stderr := cmd.ErrOrStderr()
			paths := opts.paths()
			// Configure the hub from the carried URI, or tell the user to set one
			// (matching today's manual `hub init` step) — never silently skip.
			if code.HasHubURI() {
				if err := rewriteConfigHub(paths, code.HubURI); err != nil {
					return err
				}
				opts.progressf(stderr, "Configured hub: %s\n", code.HubURI)
			} else {
				opts.progressf(stderr, "note: the pairing code carried no hub; set one before syncing — run 'devstrap hub init <url>' (or edit hub: in %s), then 'devstrap sync'\n", filepath.Join(paths.Home, "config.yaml"))
			}

			// Generate THIS device's own pairing code for the founder to approve
			// (join can't deliver it — this stays a human-relayed paste).
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			blob, fp, err := buildLocalPairingCode(cmd.Context(), opts, store)
			if err != nil {
				return err
			}
			// The returned code is the essential actionable result of `join`, so
			// it prints to stdout unconditionally — never fully suppressed by
			// --quiet (P7-CLI-03). The guidance around it is progress on stderr.
			if _, err := fmt.Fprintln(stdout, blob); err != nil {
				return err
			}
			opts.progressf(stderr, "Joined workspace %s and pinned the founder.\nRelay the code above to the founding device and have it run:\n  devstrap devices enroll --code '<paste>' --approve --fingerprint <this device's fingerprint>\nThen run 'devstrap sync' here once the founder has synced the key grant.\nThis device's fingerprint (optional high-assurance; read aloud to the founder):\n  %s\n", code.WorkspaceID, fp)
			return nil
		},
	}
	cmd.Flags().StringVar(&fingerprint, "fingerprint", "", "the founder's fingerprint confirmed out-of-band; when set, it must match the code's embedded/derived value (high-assurance — defends a compromised paste channel)")
	cmd.Flags().StringVar(&workspaceName, "workspace-name", "", "workspace name")
	return cmd
}
