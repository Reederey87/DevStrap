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
// `hub init` when the code carries a REMOTE hub URI, and generating this
// device's own pairing code for the founder to approve.
//
// Fingerprint trust: a v2 code carries the founder's fingerprint, so join
// auto-trusts it by default (no prompt) — this trusts the paste channel, it is
// NOT cryptographic authentication. Passing --fingerprint enforces the
// high-assurance out-of-band compare (constant-time; refuses on mismatch). A v1
// code (no embedded fingerprint) falls back to init --join --code's existing
// TTY-prompt / non-TTY pending behavior.
//
// Hub trust: the pairing blob is unauthenticated, so a carried `file:`/
// `folder:` hub URI is never auto-applied — it would silently point this
// device's sync at an attacker-chosen local filesystem path under a
// compromised paste channel. Only remote schemes (r2://, s3://, git+ssh://,
// git@host:path) are auto-configured; a local-scheme URI is reported but left
// for the operator to apply by hand via `hub init` if they want it.
// joinResult is the --json shape for `devstrap join` (P5-CLI-01 part B). Unlike
// `up`, join does not call any other already-self-rendering command, so it owns
// its own single render at the end of RunE (after runInit's internal render has
// been suppressed via initParams.calledFromJoin).
type joinResult struct {
	WorkspaceID   string `json:"workspace_id"`
	FounderPinned bool   `json:"founder_pinned"`
	HubConfigured bool   `json:"hub_configured,omitempty"`
	Code          string `json:"code"`
	Fingerprint   string `json:"fingerprint"`
}

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
			var pinnedFounder bool
			if err := runInit(cmd, nil, stdout, opts, initParams{
				workspaceName:    workspaceName,
				join:             true,
				codeBlob:         codeBlob,
				fingerprint:      fingerprint,
				autoTrustFounder: autoTrust,
				calledFromJoin:   true,
				pinnedFounderOut: &pinnedFounder,
			}); err != nil {
				return err
			}

			stderr := cmd.ErrOrStderr()
			paths := opts.paths()
			// Configure the hub from the carried URI, but only for a remote
			// scheme — a local file:/folder: target from an unauthenticated
			// blob is never applied automatically (see the doc comment above).
			hubConfigured := false
			switch {
			case code.HasHubURI() && isLocalHubURI(code.HubURI):
				opts.progressf(stderr, "note: the pairing code carried a local hub (%s); local hubs are never auto-configured from a pairing code (a compromised paste channel could redirect your sync) — run 'devstrap hub init %s' yourself to apply it, or 'devstrap sync' once you've set a hub\n", code.HubURI, code.HubURI)
			case code.HasHubURI():
				if err := rewriteConfigHub(paths, code.HubURI); err != nil {
					return err
				}
				opts.progressf(stderr, "Configured hub: %s\n", code.HubURI)
				hubConfigured = true
			default:
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
			result := joinResult{
				WorkspaceID:   code.WorkspaceID,
				FounderPinned: pinnedFounder,
				HubConfigured: hubConfigured,
				Code:          blob,
				Fingerprint:   fp,
			}
			human := func(w io.Writer) error {
				if _, err := fmt.Fprintln(w, blob); err != nil {
					return err
				}
				status := "the founder is still pending approval"
				if pinnedFounder {
					status = "pinned the founder"
				}
				hubNote := ""
				if !hubConfigured {
					hubNote = " Configure a hub before syncing if one wasn't applied above."
				}
				opts.progressf(stderr, "Joined workspace %s; %s.%s\nRelay the code above to the founding device and have it run:\n  devstrap devices enroll --code '<paste>' --approve --fingerprint <this device's fingerprint>\nThen run 'devstrap sync' here once the founder has synced the key grant.\nThis device's fingerprint (optional high-assurance; read aloud to the founder):\n  %s\n", code.WorkspaceID, status, hubNote, fp)
				return nil
			}
			return opts.render(stdout, human, result)
		},
	}
	cmd.Flags().StringVar(&fingerprint, "fingerprint", "", "the founder's fingerprint confirmed out-of-band; when set, it must match the code's embedded/derived value (high-assurance — defends a compromised paste channel)")
	cmd.Flags().StringVar(&workspaceName, "workspace-name", "", "workspace name")
	return cmd
}

// isLocalHubURI reports whether uri names a local-filesystem hub backend
// (file: a single flat-file test hub, folder: a local directory carrier) —
// the schemes join refuses to auto-apply from an unauthenticated pairing
// code. Remote schemes (r2://, s3://, git+ssh://, git@host:path) are safe to
// auto-apply: they name a shared remote resource, not an arbitrary local path.
func isLocalHubURI(uri string) bool {
	return strings.HasPrefix(uri, "file:") || strings.HasPrefix(uri, "folder:")
}
