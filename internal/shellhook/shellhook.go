// Package shellhook holds the per-shell init snippets `devstrap shell-init`
// prints. Each snippet WRAPS the shell's existing prompt-hook mechanism
// (bash's PROMPT_COMMAND array, zsh's precmd_functions array, fish's
// fish_prompt event) instead of replacing it outright.
//
// Replacing an existing precmd/PROMPT_COMMAND/fish_prompt handler is a
// well-documented breakage class: it broke Starship when direnv's tcsh hook
// overwrote precmd, and Starship's fix was to wrap via USER_PRECMD/
// USER_POSTCMD instead of clobbering. Every snippet here follows the same
// append-don't-replace discipline so devstrap's hook composes with whatever
// prompt framework (Starship, Powerlevel10k, oh-my-zsh, direnv, ...) the user
// already has installed, in either install order.
package shellhook

import "fmt"

// bashInit appends a function call to the PROMPT_COMMAND array. Bash >=5.1
// treats PROMPT_COMMAND as an array-typed special variable and runs every
// element on each prompt render, so `+=` always appends a new element rather
// than overwriting anything already registered there (by direnv, Starship,
// etc.) — whether PROMPT_COMMAND was previously a plain string, an array, or
// unset. (On Bash <5.1 — notably macOS's system `/bin/bash`, which stays at
// 3.2 for licensing reasons — a PROMPT_COMMAND array is still *populated*
// correctly by this snippet, since array-append itself is much older than
// 5.1, but the shell's own prompt machinery only auto-runs element 0: an
// existing hook already in slot 0 keeps working exactly as before, but
// devstrap's own hook silently never fires by itself. That degrades safely
// — no other hook is clobbered — but it does mean the integration needs a
// newer bash (`brew install bash`) or zsh to actually populate
// `$DEVSTRAP_PROMPT`.) The `case` guard makes re-sourcing this snippet
// (e.g. a second `eval "$(devstrap shell-init bash)"` in the same shell)
// idempotent instead of re-appending a duplicate entry on every source.
const bashInit = `# devstrap shell integration (bash)
_devstrap_prompt_precmd() {
  DEVSTRAP_PROMPT="$(devstrap status --prompt 2>/dev/null)"
}
case "${PROMPT_COMMAND[*]-}" in
  *_devstrap_prompt_precmd*) ;;
  *) PROMPT_COMMAND+=(_devstrap_prompt_precmd) ;;
esac
`

// zshInit appends a function to zsh's own multi-hook precmd_functions array,
// rather than assigning the precmd() function directly (which would silently
// discard any other tool's precmd()). The `case` guard makes re-sourcing this
// snippet idempotent, mirroring bashInit.
const zshInit = `# devstrap shell integration (zsh)
_devstrap_prompt_precmd() {
  DEVSTRAP_PROMPT="$(devstrap status --prompt 2>/dev/null)"
}
case "${precmd_functions[*]-}" in
  *_devstrap_prompt_precmd*) ;;
  *) precmd_functions+=(_devstrap_prompt_precmd) ;;
esac
`

// fishInit defines a new function bound to the fish_prompt event instead of
// redefining fish_prompt itself. Fish natively supports any number of
// functions bound to the same event, so this never clobbers another tool's
// binding; re-sourcing this snippet is already idempotent because fish keys
// event bindings by function name, so redefining `_devstrap_prompt_precmd`
// replaces its own prior binding rather than adding a second one. `-g`
// (global, not `-gx`/exported) matches bash/zsh: DEVSTRAP_PROMPT is a shell
// variable for the prompt to read, not something that should leak into every
// child process this shell spawns.
const fishInit = `# devstrap shell integration (fish)
function _devstrap_prompt_precmd --on-event fish_prompt
    set -g DEVSTRAP_PROMPT (devstrap status --prompt 2>/dev/null)
end
`

// Shells lists the supported shell names, in the order `shell-init --help`
// should present them.
func Shells() []string { return []string{"bash", "zsh", "fish"} }

// Script returns the eval-able init snippet for the named shell.
func Script(shell string) (string, error) {
	switch shell {
	case "bash":
		return bashInit, nil
	case "zsh":
		return zshInit, nil
	case "fish":
		return fishInit, nil
	default:
		return "", fmt.Errorf("unsupported shell %q (want bash, zsh, or fish)", shell)
	}
}
