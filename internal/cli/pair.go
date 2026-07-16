package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Reederey87/DevStrap/internal/pairing"
	"github.com/Reederey87/DevStrap/internal/state"
	"github.com/spf13/cobra"
)

// pairDefaultTimeout bounds how long `devstrap pair` blocks on the operator's
// paste before exiting cleanly with the manual follow-up, so a wizard left open
// and forgotten (or started in an unattended context) never hangs forever.
const pairDefaultTimeout = 15 * time.Minute

// newPairCommand implements `devstrap pair` (P7-PROD-01 slice 2): a founder-side
// interactive wizard that automates the founder's half of the two-device
// pairing ceremony. It assumes the local device is an already-founded workspace
// (it does NOT bootstrap — that is `devstrap up`) and orchestrates the shipped
// `devices pairing-code` + `devices enroll --code --approve` + `sync` logic:
//
//  1. print THIS device's pairing code + fingerprint (buildLocalPairingCode);
//  2. print the exact command the second device runs (`devstrap join '<code>'`);
//  3. block reading ONE line of stdin (the joiner's code pasted back), bounded
//     by --timeout and interruptible by SIGINT (ctx cancellation);
//  4. a blank/EOF line or timeout exits cleanly with the manual follow-up — it
//     is "not ready yet / finishing manually", never an error;
//  5. a decoded code is confirmed + approved via the EXACT same
//     confirmDeviceFingerprint path `devices enroll --approve` uses (through
//     runDeviceEnroll), sharing the wizard's single stdin reader so the "yes"
//     confirmation reads correctly after the pasted code;
//  6. `sync` publishes the key grant.
//
// It is orchestration only: no new crypto, no new wire format. Interactive paste
// requires a TTY; a non-TTY invocation fails FAST with the manual-flow remedy
// rather than hanging on input that will never arrive.
func newPairCommand(stdout io.Writer, opts *options) *cobra.Command {
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "pair",
		Short: "Guided founder-side wizard for pairing a second device (P7-PROD-01)",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			stderr := cmd.ErrOrStderr()
			paths := opts.paths()

			// Only 0 (documented as "wait indefinitely") legitimately disables the
			// timer; a negative duration is a usage mistake, not another way to
			// spell "forever" (review finding, PR #202).
			if timeout < 0 {
				return appError{code: exitUsage, err: fmt.Errorf("--timeout must be >= 0 (0 waits indefinitely)")}
			}

			// A friendly refusal for a home that was never initialized, before the
			// raw sqlite "unable to open database file" surfaces.
			if _, err := os.Stat(paths.StateDB()); errors.Is(err, os.ErrNotExist) {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("no workspace here yet: run 'devstrap up --hub <url>' (or 'devstrap init') on this device first")}
			}

			store, err := opts.openState(ctx)
			if err != nil {
				return err
			}
			defer closeStore(store)

			// Founder-only: refuse cleanly on an uninitialized store, and refuse a
			// joiner (its half of the ceremony is `devstrap join`, not `pair`).
			localWS, err := store.WorkspaceID(ctx)
			if err != nil {
				if errors.Is(err, state.ErrNotInitialized) {
					return appError{code: exitInvalidConfig, err: fmt.Errorf("no workspace here yet: run 'devstrap up --hub <url>' (or 'devstrap init') on this device first")}
				}
				return err
			}
			if isJoiner(opts) {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("this device joined an existing workspace (role: joiner); 'devstrap pair' is the FOUNDER's wizard — run 'devstrap join <code>' here instead, or run 'devstrap pair' on the founding device")}
			}
			localDevice, err := store.CurrentDevice(ctx)
			if err != nil {
				return err
			}

			// Step 1+2: print this device's code (stdout, unconditional per
			// P7-CLI-03) + the exact command the second device should run.
			blob, fp, err := buildLocalPairingCode(ctx, opts, store)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintln(stdout, blob); err != nil {
				return err
			}
			opts.progressf(stderr, "Your pairing code is printed above. On the SECOND device, run:\n  devstrap join '%s'\n  # add --fingerprint %s to also verify it out-of-band (defends a compromised paste channel)\nThis device's fingerprint (optional high-assurance; read aloud over a trusted channel):\n  %s\n\n", blob, fp, fp)

			// Interactive paste requires a real terminal; a non-TTY invocation must
			// fail fast rather than hang on input that will never come.
			if !stdinIsTerminal(cmd) {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("'devstrap pair' needs an interactive terminal to read the second device's code.\nIn a script/CI, use the manual flow instead:\n  on the second device: devstrap join '<this code>'   (or devstrap devices pairing-code)\n  here: devstrap devices enroll --code '<its code>' --approve --fingerprint <its fingerprint>\n  here: devstrap sync")}
			}

			// Step 3: block on ONE line of stdin, bounded by --timeout and
			// interruptible by SIGINT (ctx). A single shared bufio.Reader is reused
			// for the enroll confirmation below, so a pre-buffered "yes" is not lost.
			reader := bufio.NewReader(cmd.InOrStdin())
			opts.progressf(stderr, "Paste the second device's pairing code here once you have it (Ctrl-C to finish manually later): ")

			type pasteResult struct {
				line string
				err  error
			}
			resCh := make(chan pasteResult, 1) // buffered: the reader goroutine can send and exit even after we stopped waiting
			go func() {
				line, rerr := reader.ReadString('\n')
				resCh <- pasteResult{line: line, err: rerr}
			}()

			var timeoutCh <-chan time.Time
			if timeout > 0 {
				timer := time.NewTimer(timeout)
				defer timer.Stop()
				timeoutCh = timer.C
			}

			var pasted string
			select {
			case res := <-resCh:
				if res.err != nil && !errors.Is(res.err, io.EOF) {
					return fmt.Errorf("read the second device's pairing code: %w", res.err)
				}
				pasted = res.line
			case <-timeoutCh:
				opts.progressf(stderr, "\nTimed out waiting for the second device's code.\n")
				printPairManualFollowup(opts, stderr, fp)
				return nil
			case <-ctx.Done():
				opts.progressf(stderr, "\nStopped before a code was pasted.\n")
				printPairManualFollowup(opts, stderr, fp)
				return nil
			}

			// Step 4: a blank/whitespace-only line or bare EOF is "not ready yet /
			// finishing manually", not an error — exit cleanly with the follow-up.
			codeBlob := strings.TrimSpace(pasted)
			if codeBlob == "" {
				opts.progressf(stderr, "\nNo code entered.\n")
				printPairManualFollowup(opts, stderr, fp)
				return nil
			}

			// Step 5: decode + confirm + approve the joiner. Reuse the enroll path
			// (runDeviceEnroll → confirmDeviceFingerprint) with the shared reader.
			code, err := pairing.Decode(codeBlob)
			if err != nil {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("that is not a valid pairing code: %w", err)}
			}
			if code.WorkspaceID != localWS {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("that pairing code is for workspace %s, but this device is %s — it was generated on a device from a different workspace", code.WorkspaceID, localWS)}
			}
			// A fat-fingered paste of THIS device's own code (printed just above
			// the prompt) would otherwise proceed into runDeviceEnroll and
			// re-approve/re-grant this device to itself — benign but confusing.
			// Refuse it with a clear message instead (review finding, PR #202).
			if code.DeviceID == localDevice.ID {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("that is this device's OWN pairing code (printed above) — paste the SECOND device's code instead, once you've run 'devstrap join' there")}
			}
			if err := runDeviceEnroll(cmd, reader, stdout, opts, store, code.DeviceID, code.Name, code.OS, code.Arch, code.AgeRecipient, code.SigningPublicKey, true, false, ""); err != nil {
				return err
			}

			// Step 6: publish the grant. Reads the hub from config (set by
			// `devstrap up`/`hub init`); surfaces sync's own error unwrapped.
			opts.progressf(stderr, "Syncing to publish the key grant…\n")
			if err := runSyncCycle(ctx, stdout, stderr, opts, "", false, false); err != nil {
				opts.progressf(stderr, "pair: device %s is approved (safe to keep); only the grant-publishing sync failed. Re-run 'devstrap sync' to publish it.\n", code.DeviceID)
				return err
			}

			opts.progressf(stdout, "\nPaired: device %s approved in workspace %s; this device synced to publish the key grant.\nThe second device must now run 'devstrap sync' once to receive the grant and materialize the tree.\n", code.DeviceID, localWS)
			return nil
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", pairDefaultTimeout, "how long to wait for the second device's pasted code before exiting cleanly (0 waits indefinitely)")
	return cmd
}

// printPairManualFollowup prints the manual next steps when the wizard finishes
// without a pasted code (blank line, EOF, timeout, or interruption), so the
// operator can complete the founder side by hand later.
func printPairManualFollowup(opts *options, stderr io.Writer, fingerprint string) {
	opts.progressf(stderr, "Finish the founder side by hand later:\n  on the second device: devstrap join '<this code>'   (verify with --fingerprint %s if you want the out-of-band check)\n  here: devstrap devices enroll --code '<its code>' --approve --fingerprint <its fingerprint>\n  here: devstrap sync\n", fingerprint)
}
