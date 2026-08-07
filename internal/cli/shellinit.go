package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/Reederey87/DevStrap/internal/shellhook"
	"github.com/spf13/cobra"
)

// newShellInitCommand implements `devstrap shell-init {bash|zsh|fish}`: it
// prints eval-able shell code that wires `devstrap status --prompt` into the
// shell's own prompt-hook mechanism (W12-01). The emitted code always WRAPS
// whatever hook the shell already has instead of replacing it — see
// internal/shellhook's package doc for why that discipline matters.
//
// The output is shell source, not data, so — unlike most commands here — it
// deliberately does not route through opts.render/--json.
func newShellInitCommand(stdout io.Writer, _ *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "shell-init {bash|zsh|fish}",
		Short: "Print shell code that wires `devstrap status --prompt` into your prompt",
		Long: `Print shell code that wires devstrap's fast, local-only status summary into
your shell prompt.

Wire it into your shell's startup file:

  echo 'eval "$(devstrap shell-init zsh)"'  >> ~/.zshrc
  echo 'eval "$(devstrap shell-init bash)"' >> ~/.bashrc
  echo 'devstrap shell-init fish | source'  >> ~/.config/fish/config.fish

The installed hook appends to (bash's PROMPT_COMMAND array / zsh's
precmd_functions array) or adds a new fish_prompt event handler — it never
replaces an existing hook, so it composes with Starship, direnv, oh-my-zsh,
and similar tools regardless of install order. It sets $DEVSTRAP_PROMPT on
every prompt render; embed that in your PS1/PROMPT/fish_prompt, or point a
Starship "custom" module at ` + "`devstrap status --prompt`" + ` directly.`,
		Args:      usageArgs(cobra.ExactArgs(1)),
		ValidArgs: shellhook.Shells(),
		RunE: func(cmd *cobra.Command, args []string) error {
			script, err := shellhook.Script(args[0])
			if err != nil {
				return appError{code: exitUsage, err: fmt.Errorf("%w (supported: %s)", err, strings.Join(shellhook.Shells(), ", "))}
			}
			_, err = fmt.Fprint(stdout, script)
			return err
		},
	}
	return cmd
}
