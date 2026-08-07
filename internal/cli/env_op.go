package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/Reederey87/DevStrap/internal/childenv"
	"github.com/Reederey87/DevStrap/internal/secrets/onepassword"
	"github.com/Reederey87/DevStrap/internal/state"
	"github.com/spf13/cobra"
)

// newEnvOpCommand groups the 1Password browse/write-back commands (W12-03):
// `env op list` discovers existing items as copyable op:// references instead
// of requiring them to be hand-typed, and `env op set` either binds an
// already-known op:// reference (exactly the `env bind` write path) or writes
// a new/changed plaintext value into 1Password first, then binds the
// resulting reference — never storing the plaintext value itself.
func newEnvOpCommand(stdout io.Writer, opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "op",
		Short: "Browse and write 1Password secrets, storing only op:// references",
	}
	cmd.AddCommand(newEnvOpListCommand(stdout, opts))
	cmd.AddCommand(newEnvOpSetCommand(stdout, opts))
	return cmd
}

// requireOpCLI gates every `env op` subcommand behind the `op` binary being
// on PATH. Unlike the gh/glab/tea forge CLIs (FORGE-01, createForgePR), there
// is no manual fallback for `env op` — browsing and writing 1Password items
// has no path that doesn't go through `op` — so this is a hard error
// (exitInvalidConfig), not the print-a-workaround-and-exit-0 degradation
// createForgePR uses. The message itself mirrors the established
// missing-`op` wording (internal/cli/hub.go's resolveOpRef) rather than
// inventing new phrasing.
func requireOpCLI() error {
	if err := onepassword.LookPath(); err != nil {
		return appError{code: exitInvalidConfig, err: err}
	}
	return nil
}

// envOpListResult is the --json shape for `env op list`. Refs are copyable
// op://vault/item/field pointers only — no field ever carries a value.
type envOpListResult struct {
	Refs []string `json:"refs"`
}

func newEnvOpListCommand(stdout io.Writer, opts *options) *cobra.Command {
	var vault string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List 1Password items as copyable op://vault/item/field references (never prints values)",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireOpCLI(); err != nil {
				return err
			}
			ctx := cmd.Context()
			items, err := onepassword.ListItems(ctx, vault)
			if err != nil {
				return appError{code: exitGit, err: err}
			}
			var refs []string
			for _, it := range items {
				fields, err := onepassword.ListFields(ctx, it.ID)
				if err != nil {
					return appError{code: exitGit, err: err}
				}
				vaultName := it.Vault.Name
				if vaultName == "" {
					vaultName = it.Vault.ID
				}
				for _, f := range fields {
					name := f.Label
					if name == "" {
						name = f.ID
					}
					if name == "" || vaultName == "" || it.Title == "" {
						continue
					}
					refs = append(refs, fmt.Sprintf("op://%s/%s/%s", vaultName, it.Title, name))
				}
			}
			sort.Strings(refs)
			result := envOpListResult{Refs: refs}
			return opts.render(stdout, func(w io.Writer) error {
				if len(refs) == 0 {
					_, err := fmt.Fprintln(w, "No 1Password items found.")
					return err
				}
				for _, ref := range refs {
					if _, err := fmt.Fprintln(w, ref); err != nil {
						return err
					}
				}
				return nil
			}, result)
		},
	}
	cmd.Flags().StringVar(&vault, "vault", "", "limit browsing to a single 1Password vault")
	return cmd
}

// envOpSetResult is the --json shape for `env op set`. Ref is the resulting
// op:// pointer, never the value that produced it.
type envOpSetResult struct {
	Path string `json:"path"`
	Key  string `json:"key"`
	Ref  string `json:"ref"`
}

var envOpKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func newEnvOpSetCommand(stdout io.Writer, opts *options) *cobra.Command {
	var profileName, vault, item, field string
	cmd := &cobra.Command{
		Use:   "set <path> <key> <value-or-op-ref>",
		Short: "Point one env variable at a 1Password reference, writing a new value into 1Password first if given plaintext",
		Long: `Point one project env variable at a 1Password reference.

If <value-or-op-ref> already starts with op://, it is bound as-is (the same
write path 'env bind' uses) -- no 1Password write happens.

Otherwise <value-or-op-ref> is treated as a plaintext value to write into
1Password. Prefer "-" to read it from stdin instead: passing the value
directly on the command line leaves it in devstrap's own shell history (and
briefly visible to other local processes via the process list), which passing
it inline into 'op' itself would too -- devstrap never does that (see below).
The value is written into the target item via a private, mode-0600 JSON
template file passed to 'op item edit --template=<file>' -- 1Password's own
CLI best-practices guidance warns that an inline 'field=value' assignment is
visible in shell history and to other local processes, so that shape is never
used against 'op'. devstrap prints the exact op://vault/item/field it is
about to overwrite before writing.

The target item/vault/field default to whatever <key> is already bound to (a
rotate-in-place, overwriting that field's current value); writing a brand new
key's plaintext value requires --vault and --item. An explicit --field always
wins over the existing binding's field, intentionally rebinding <key> to a
different field on the same item rather than silently ignoring the flag.`,
		Args: usageArgs(cobra.ExactArgs(3)),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := requireOpCLI(); err != nil {
				return err
			}
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)

			project, err := store.ProjectByPath(cmd.Context(), args[0])
			if err != nil {
				return err
			}

			key := strings.TrimSpace(args[1])
			if !envOpKeyPattern.MatchString(key) {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("invalid variable name %q", key)}
			}
			if childenv.Dangerous(key) {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("refusing dangerous variable name %q", key)}
			}

			existingRefs, err := providerRefsForUpdate(cmd.Context(), store, project)
			if err != nil {
				return err
			}

			announceWrite := func(v, i, f string) {
				// Transparency before a mutating call (Fable UX review,
				// W12-03): a plaintext write can silently overwrite whatever
				// currently sits in that 1Password field -- name the exact
				// target before it happens rather than only after.
				opts.progressf(cmd.ErrOrStderr(), "Writing %s into 1Password op://%s/%s/%s...\n", key, v, i, f)
			}
			ref, wroteToOnePassword, err := resolveOpSetRef(cmd.Context(), cmd.InOrStdin(), args[2], key, existingRefs, vault, item, field, announceWrite)
			if err != nil {
				return err
			}

			refs := make(map[string]string, len(existingRefs)+1)
			for k, r := range existingRefs {
				refs[k] = r
			}
			refs[key] = ref

			if err := bindProviderRefs(cmd.Context(), store, project, profileName, "1password", refs); err != nil {
				if wroteToOnePassword {
					// The value is already written to 1Password even though the
					// local bind just failed -- there is no distributed
					// transaction across `op` and state.db. Name the ref so the
					// operator can recover with a follow-up `env op set` call
					// that hits the op:// (no-write) branch instead of
					// re-typing the plaintext (W12-03 review).
					return fmt.Errorf("wrote %s to 1Password but failed to record the local binding (retry with `devstrap env op set %s %s %s`): %w", ref, project.Path, key, ref, err)
				}
				return err
			}

			result := envOpSetResult{Path: project.Path, Key: key, Ref: ref}
			return opts.render(stdout, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "Set %s for %s to %s\n", key, project.Path, ref)
				return err
			}, result)
		},
	}
	cmd.Flags().StringVar(&profileName, "profile", "default", "env profile name")
	cmd.Flags().StringVar(&vault, "vault", "", "1Password vault (required with --item to write a brand new plaintext value)")
	cmd.Flags().StringVar(&item, "item", "", "1Password item (required with --vault to write a brand new plaintext value)")
	cmd.Flags().StringVar(&field, "field", "", "1Password field label (defaults to the key name, or the existing binding's field)")
	return cmd
}

// resolveOpSetRef implements `env op set`'s value-vs-ref branch. An op://...
// argument is bound as-is with no 1Password write (wrote=false). Anything
// else is a plaintext value ("-" meaning "read it from stdin") written into
// 1Password via onepassword.SetField (wrote=true), defaulting the target
// vault/item/field to the key's existing binding (a rotate-in-place) when
// --vault/--item/--field are not given -- an EXPLICIT --field always wins
// over the existing binding's field, intentionally rebinding the key to a
// different field on the same item rather than silently ignoring the flag.
// The caller uses wrote to phrase a partial-success error correctly if the
// subsequent local bind fails. announceWrite (vault, item, field), if
// non-nil, is called immediately before the 1Password write so the caller
// can tell the operator exactly what is about to be overwritten (Fable UX
// review, W12-03) -- it is never called on the op://-ref branch, since that
// branch performs no write.
func resolveOpSetRef(ctx context.Context, stdin io.Reader, valueArg, key string, existingRefs map[string]string, vault, item, field string, announceWrite func(vault, item, field string)) (ref string, wrote bool, err error) {
	if strings.HasPrefix(valueArg, "op://") {
		if _, _, _, perr := parseOpRef(valueArg); perr != nil {
			return "", false, appError{code: exitInvalidConfig, err: perr}
		}
		return valueArg, false, nil
	}

	value := valueArg
	if valueArg == "-" {
		v, serr := readStdinValue(stdin)
		if serr != nil {
			return "", false, serr
		}
		value = v
	}

	if existing, ok := existingRefs[key]; ok {
		if ev, ei, ef, perr := parseOpRef(existing); perr == nil {
			if vault == "" {
				vault = ev
			}
			if item == "" {
				item = ei
			}
			if field == "" {
				field = ef
			}
		}
	}
	if field == "" {
		field = key
	}
	if vault == "" || item == "" {
		return "", false, appError{code: exitUsage, err: fmt.Errorf("%s has no existing op:// binding; pass --vault and --item to write a new value into 1Password", key)}
	}

	if announceWrite != nil {
		announceWrite(vault, item, field)
	}
	ref, err = onepassword.SetField(ctx, vault, item, field, value)
	if err != nil {
		return "", false, appError{code: exitGit, err: err}
	}
	return ref, true, nil
}

// providerRefsForUpdate returns the current 1Password provider refs for a
// project's env profile, so `env op set` can merge a single key into the full
// map the store requires (UpsertEnvProfileTx always replaces the complete ref
// set -- there is no incremental-update path, matching `env bind`'s existing
// full-refs-file-replace behavior). Absent any profile (state.ErrEnvProfileNotFound)
// it returns an empty map; every OTHER error propagates. This distinction
// matters here in a way it would not for a read-only fact check (cf.
// export.go's hasEnvProfile, which treats any error as "no profile" because
// it only ever reports a boolean): providerRefsForUpdate feeds a WRITE, so
// misreading a transient read failure as "no profile" would make `env op
// set` silently replace an existing profile's full ref set with just the one
// key being set (W12-03 review). An existing profile using a different
// provider (e.g. devstrap_encrypted from `env capture`) refuses rather than
// silently stranding it: UpsertEnvProfileTx repoints the project's single env
// profile slot, so writing provider refs here would overwrite that encrypted
// profile's pointer.
func providerRefsForUpdate(ctx context.Context, store *state.Store, project state.ProjectStatus) (map[string]string, error) {
	profile, bindings, err := store.EnvProfileForProject(ctx, project.ID)
	if errors.Is(err, state.ErrEnvProfileNotFound) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	if profile.Provider != "1password" {
		return nil, appError{code: exitInvalidConfig, err: fmt.Errorf("%s already has a %s env profile; `env op set` only manages 1password provider refs (use --profile to target a different profile name)", project.Path, profile.Provider)}
	}
	refs := make(map[string]string, len(bindings))
	for _, b := range bindings {
		if b.ProviderRef != "" {
			refs[b.VarName] = b.ProviderRef
		}
	}
	return refs, nil
}

// errInvalidOpRef never echoes the offending string. A value that looks
// enough like an op:// reference to reach this function (it has the op://
// prefix) but does not parse could, in principle, be a plaintext value that
// merely happens to start with that substring; the argument itself is not
// worth reflecting back into an error message when a fixed, generic message
// says exactly as much (W12-03 review).
var errInvalidOpRef = errors.New("invalid op reference, want op://vault/item/field")

// parseOpRef splits an op://vault/item/field reference into its three parts.
func parseOpRef(ref string) (vault, item, field string, err error) {
	rest, ok := strings.CutPrefix(ref, "op://")
	if !ok {
		return "", "", "", errInvalidOpRef
	}
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", errInvalidOpRef
	}
	return parts[0], parts[1], parts[2], nil
}

// maxStdinValueBytes bounds `env op set <path> <key> -`'s stdin read. It is
// deliberately generous for a single env value while still bounded.
const maxStdinValueBytes = 1 << 20

// readStdinValue reads a plaintext value from stdin for `env op set <path>
// <key> -`, so a caller can avoid the value ever appearing as a bare argument
// in devstrap's own shell history. It reads one byte past the limit so an
// oversized value is reported explicitly rather than silently truncated
// (W12-03 review), and trims exactly one trailing line ending (as added by
// `echo` or a text editor) rather than every trailing CR/LF byte, so the
// value is otherwise passed through unmodified.
func readStdinValue(r io.Reader) (string, error) {
	raw, err := io.ReadAll(io.LimitReader(r, maxStdinValueBytes+1))
	if err != nil {
		return "", fmt.Errorf("read value from stdin: %w", err)
	}
	if len(raw) > maxStdinValueBytes {
		return "", appError{code: exitInvalidConfig, err: fmt.Errorf("value on stdin exceeds the %d byte limit", maxStdinValueBytes)}
	}
	value := string(raw)
	value = strings.TrimSuffix(value, "\n")
	value = strings.TrimSuffix(value, "\r")
	return value, nil
}
