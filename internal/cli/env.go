package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Reederey87/DevStrap/internal/config"
	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/envbundle"
	"github.com/Reederey87/DevStrap/internal/envfile"
	"github.com/Reederey87/DevStrap/internal/platform"
	"github.com/Reederey87/DevStrap/internal/state"
	"github.com/spf13/cobra"
)

func newEnvCommand(stdout io.Writer, opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "env",
		Short: "Manage project environment profiles",
	}
	cmd.AddCommand(newEnvCaptureCommand(stdout, opts))
	cmd.AddCommand(newEnvHydrateCommand(stdout, opts))
	cmd.AddCommand(newEnvBindCommand(stdout, opts))
	cmd.AddCommand(newEnvRotateCommand(stdout, opts))
	return cmd
}

// captureEnvProfile reads, parses, encrypts, and stores a project's env file as
// an age-encrypted blob, returning the number of bindings and the blob ref. It
// is shared by `env capture` and `env rotate` (P5-PROD-03).
func captureEnvProfile(ctx context.Context, store *state.Store, opts *options, project state.ProjectStatus, envFile, profileName string, literal bool) (nBindings int, ref string, nRecipients int, err error) {
	// P5 review: enforce the git_repo guard in the shared helper so both
	// `env capture` and `env rotate` reject non-git projects consistently.
	if project.Type != "git_repo" {
		return 0, "", 0, appError{code: exitInvalidConfig, err: fmt.Errorf("%s is %s, not git_repo", project.Path, project.Type)}
	}
	localPath := project.LocalPath
	if localPath == "" {
		localPath = filepath.Join(opts.paths().Root, filepath.FromSlash(project.Path))
	}
	envPath := envFile
	if !filepath.IsAbs(envPath) {
		envPath = filepath.Join(localPath, envPath)
	}
	raw, err := readEnvFile(envPath)
	if err != nil {
		return 0, "", 0, err
	}
	bindings, err := envfile.ParseBytes(raw, envfile.Options{Literal: literal})
	if err != nil {
		return 0, "", 0, appError{code: exitInvalidConfig, err: err}
	}
	device, err := store.CurrentDevice(ctx)
	if err != nil {
		return 0, "", 0, err
	}
	recipients, err := envRecipients(ctx, store, device)
	if err != nil {
		return 0, "", 0, err
	}
	ciphertext, ref, err := envbundle.Encrypt(bindings, recipients)
	if err != nil {
		return 0, "", 0, err
	}
	if err := writeEnvBlob(opts.paths(), ref, ciphertext); err != nil {
		return 0, "", 0, err
	}
	varNames := make([]string, 0, len(bindings))
	for _, binding := range bindings {
		varNames = append(varNames, binding.Name)
	}
	if _, err := store.SaveCapturedEnvProfile(ctx, project.ID, profileName, varNames, ref); err != nil {
		return 0, "", 0, err
	}
	if err := ensureIgnored(localPath, envPath); err != nil {
		return 0, "", 0, err
	}
	return len(bindings), ref, len(recipients), nil
}

func newEnvRotateCommand(stdout io.Writer, opts *options) *cobra.Command {
	var literal bool
	var profileName string
	var all bool
	cmd := &cobra.Command{
		Use:   "rotate [path] [env-file]",
		Short: "Re-capture a rotated secret to the current recipients and clear its needs-rotation flag (P5-PROD-03)",
		Args:  cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := opts.openState(cmd.Context())
			if err != nil {
				return err
			}
			defer closeStore(store)
			if all {
				if len(args) != 0 {
					return appError{code: exitUsage, err: fmt.Errorf("--all takes no path argument")}
				}
				cleared, err := store.ClearAllBindingRotation(cmd.Context())
				if err != nil {
					return err
				}
				_, err = fmt.Fprintf(stdout, "Cleared the needs-rotation flag on %d binding(s). Rotate the secrets at their source first.\n", cleared)
				return err
			}
			if len(args) == 0 {
				return appError{code: exitUsage, err: fmt.Errorf("pass a <path> (and optionally an env-file to re-capture) or --all")}
			}
			project, err := store.ProjectByPath(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if len(args) == 2 {
				// Re-capture the rotated value to the current recipient set.
				n, ref, nRecipients, err := captureEnvProfile(cmd.Context(), store, opts, project, args[1], profileName, literal)
				if err != nil {
					return err
				}
				if _, err := fmt.Fprintf(stdout, "Re-captured %d env variable(s) for %s into %s for %d recipient device(s)\n", n, project.Path, ref, nRecipients); err != nil {
					return err
				}
			}
			cleared, err := store.ClearRotationForProject(cmd.Context(), project.ID)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(stdout, "Cleared the needs-rotation flag on %d binding(s) for %s\n", cleared, project.Path)
			return err
		},
	}
	cmd.Flags().BoolVar(&literal, "literal", false, "capture interpolation-looking values as literal text")
	cmd.Flags().StringVar(&profileName, "profile", "default", "env profile name")
	cmd.Flags().BoolVar(&all, "all", false, "clear the needs-rotation flag on all bindings (after rotating at source)")
	return cmd
}

func newEnvCaptureCommand(stdout io.Writer, opts *options) *cobra.Command {
	var literal bool
	var profileName string
	cmd := &cobra.Command{
		Use:   "capture <path> <env-file>",
		Short: "Capture and encrypt a project env file",
		Args:  cobra.ExactArgs(2),
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
			if project.Type != "git_repo" {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("%s is %s, not git_repo", project.Path, project.Type)}
			}
			n, ref, nRecipients, err := captureEnvProfile(cmd.Context(), store, opts, project, args[1], profileName, literal)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(stdout, "Captured %d env variables for %s into %s for %d recipient device(s)\n", n, project.Path, ref, nRecipients)
			return err
		},
	}

	cmd.Flags().BoolVar(&literal, "literal", false, "capture interpolation-looking values as literal text")
	cmd.Flags().StringVar(&profileName, "profile", "default", "env profile name")
	return cmd
}

func envRecipients(ctx context.Context, store *state.Store, local state.Device) ([]string, error) {
	if local.PublicKey == "" {
		return nil, appError{code: exitInvalidConfig, err: fmt.Errorf("local device has no age recipient; run devstrap init")}
	}
	seen := map[string]bool{}
	recipients := []string{local.PublicKey}
	seen[local.PublicKey] = true
	devices, err := store.ListDevices(ctx)
	if err != nil {
		return nil, err
	}
	for _, device := range devices {
		if device.ID == local.ID || device.PublicKey == "" || device.TrustState != "approved" {
			continue
		}
		if !seen[device.PublicKey] {
			recipients = append(recipients, device.PublicKey)
			seen[device.PublicKey] = true
		}
	}
	return recipients, nil
}

func newEnvHydrateCommand(stdout io.Writer, opts *options) *cobra.Command {
	var writePath string
	var force bool
	cmd := &cobra.Command{
		Use:   "hydrate <path>",
		Short: "Decrypt a captured env profile to a local env file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if writePath == "" {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("--write is required")}
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
			localPath := project.LocalPath
			if localPath == "" {
				localPath = filepath.Join(opts.paths().Root, filepath.FromSlash(project.Path))
			}
			profile, bindings, err := store.EnvProfileForProject(cmd.Context(), project.ID)
			if err != nil {
				return err
			}
			target := writePath
			if !filepath.IsAbs(target) {
				target = filepath.Join(localPath, target)
			}
			// SECR-05: register the target in .gitignore BEFORE writing the
			// secret content so the file is ignored the instant it exists.
			if err := ensureIgnored(localPath, target); err != nil {
				return err
			}
			content, count, err := hydratedEnvContent(cmd.Context(), opts, store, profile, bindings, target, force)
			if err != nil {
				return err
			}
			if err := writeHydratedEnvFile(target, content, force); err != nil {
				return err
			}
			_, err = fmt.Fprintf(stdout, "Hydrated %d env variables for %s to %s\n", count, project.Path, target)
			return err
		},
	}
	cmd.Flags().StringVar(&writePath, "write", "", "write decrypted env values to this file")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing env file")
	return cmd
}

func hydratedEnvContent(ctx context.Context, opts *options, store *state.Store, profile state.EnvProfile, bindings []state.SecretBinding, target string, force bool) ([]byte, int, error) {
	header := renderEnvHeader(profile.Name)
	switch profile.Provider {
	case "devstrap_encrypted":
		ref, err := sharedEncryptedRef(bindings)
		if err != nil {
			return nil, 0, err
		}
		ciphertext, err := readEnvBlob(opts.paths(), ref)
		if err != nil {
			return nil, 0, err
		}
		device, err := store.CurrentDevice(ctx)
		if err != nil {
			return nil, 0, err
		}
		identity, err := devicekeys.NewHybridStore(opts.paths().KeyDir(), platform.Detect().Keychain).Read(ctx, device.ID)
		if err != nil {
			return nil, 0, fmt.Errorf("read local device identity: %w", err)
		}
		plaintext, err := envbundle.Decrypt(ciphertext, identity.Private)
		if err != nil {
			return nil, 0, err
		}
		content := renderDotenv(plaintext.Vars)
		return append(header, content...), len(plaintext.Vars), nil
	case "1password":
		if err := ensureHydrateTargetWritable(target, force); err != nil {
			return nil, 0, err
		}
		refsFile, cleanup, err := writeProviderRefsFile(bindings)
		if cleanup != nil {
			defer cleanup()
		}
		if err != nil {
			return nil, 0, err
		}
		content, err := injectProviderRefs(ctx, refsFile)
		if err != nil {
			return nil, 0, err
		}
		return append(header, content...), len(bindings), nil
	default:
		return nil, 0, appError{code: exitInvalidConfig, err: fmt.Errorf("env profile %s uses unsupported provider %s", profile.Name, profile.Provider)}
	}
}

func newEnvBindCommand(stdout io.Writer, opts *options) *cobra.Command {
	var profileName string
	var provider string
	cmd := &cobra.Command{
		Use:   "bind <path> <refs-file>",
		Short: "Bind provider secret references to a project env profile",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			provider = strings.ToLower(strings.TrimSpace(provider))
			if provider != "1password" {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("unsupported env provider %q", provider)}
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
			localPath := project.LocalPath
			if localPath == "" {
				localPath = filepath.Join(opts.paths().Root, filepath.FromSlash(project.Path))
			}
			refsPath := args[1]
			if !filepath.IsAbs(refsPath) {
				refsPath = filepath.Join(localPath, refsPath)
			}
			raw, err := readEnvFile(refsPath)
			if err != nil {
				return err
			}
			bindings, err := envfile.ParseBytes(raw, envfile.Options{Literal: true})
			if err != nil {
				return appError{code: exitInvalidConfig, err: err}
			}
			refs := make(map[string]string, len(bindings))
			for _, binding := range bindings {
				if !strings.HasPrefix(binding.Value, "op://") {
					return appError{code: exitInvalidConfig, err: fmt.Errorf("%s provider ref must start with op://", binding.Name)}
				}
				refs[binding.Name] = binding.Value
			}
			if _, err := store.SaveProviderEnvProfile(cmd.Context(), project.ID, profileName, provider, refs); err != nil {
				return err
			}
			if err := ensureIgnored(localPath, refsPath); err != nil {
				return err
			}
			_, err = fmt.Fprintf(stdout, "Bound %d provider refs for %s using %s\n", len(refs), project.Path, provider)
			return err
		},
	}
	cmd.Flags().StringVar(&profileName, "profile", "default", "env profile name")
	cmd.Flags().StringVar(&provider, "provider", "1password", "secret provider for refs-file")
	return cmd
}

func readEnvFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat env file: %w", err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("env file is a directory: %s", path)
	}
	if info.Size() > envfile.MaxBytes {
		return nil, fmt.Errorf("env file is %d bytes, max %d", info.Size(), envfile.MaxBytes)
	}
	//nolint:gosec // The path is an explicit user-selected env file and is bounded by stat/size checks above.
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read env file: %w", err)
	}
	return raw, nil
}

func writeEnvBlob(paths config.Paths, ref string, ciphertext []byte) (err error) {
	hash, err := envBlobHash(ref)
	if err != nil {
		return err
	}
	dir := filepath.Join(paths.Home, "blobs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create env blob dir: %w", err)
	}
	path := filepath.Join(dir, hash+".age")
	//nolint:gosec // The path is derived from the content-addressed age blob ref under the DevStrap home.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("create env blob: %w", err)
	}
	// CODE-04: observe the Close error on the success path so a truncated
	// encrypted blob is never silently reported as written.
	defer func() {
		if cErr := file.Close(); cErr != nil && err == nil {
			err = fmt.Errorf("close env blob: %w", cErr)
		}
	}()
	if _, err = file.Write(ciphertext); err != nil {
		return fmt.Errorf("write env blob: %w", err)
	}
	if err = file.Chmod(0o600); err != nil {
		return fmt.Errorf("secure env blob: %w", err)
	}
	return file.Sync()
}

func readEnvBlob(paths config.Paths, ref string) ([]byte, error) {
	hash, err := envBlobHash(ref)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(paths.Home, "blobs", hash+".age")
	//nolint:gosec // The path is derived from a validated content-addressed age blob ref under the DevStrap home.
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read env blob: %w", err)
	}
	return raw, nil
}

func envBlobHash(ref string) (string, error) {
	hash, ok := strings.CutPrefix(ref, "age_blob:")
	if !ok || hash == "" || strings.ContainsAny(hash, `/\`) {
		return "", fmt.Errorf("invalid env blob ref %q", ref)
	}
	return hash, nil
}

func sharedEncryptedRef(bindings []state.SecretBinding) (string, error) {
	if len(bindings) == 0 {
		return "", fmt.Errorf("env profile has no bindings")
	}
	ref := bindings[0].EncryptedValueRef
	if ref == "" {
		return "", fmt.Errorf("invalid env blob ref %q", ref)
	}
	for _, binding := range bindings[1:] {
		if binding.EncryptedValueRef != ref {
			return "", fmt.Errorf("env profile spans multiple encrypted blobs; hydrate does not yet support split bundles")
		}
	}
	return ref, nil
}

func renderDotenv(bindings []envfile.Binding) []byte {
	var out bytes.Buffer
	for _, binding := range bindings {
		out.WriteString(binding.Name)
		out.WriteByte('=')
		out.WriteString(quoteDotenv(binding.Value))
		out.WriteByte('\n')
	}
	return out.Bytes()
}

// renderEnvHeader produces the spec-mandated "Do not commit" header for
// hydrated env files (SECR-02).
func renderEnvHeader(profileName string) []byte {
	return []byte(fmt.Sprintf("# Generated by DevStrap. Do not commit.\n# Source profile: %s\n# Generated at: %s\n",
		profileName, time.Now().UTC().Format(time.RFC3339)))
}

func quoteDotenv(value string) string {
	// SECR-01: Prefer POSIX single-quote rendering so no downstream dotenv
	// loader (python-dotenv, ruby dotenv, dotenvx) can interpolate $ or
	// backtick. Single-quoted values are literal in every implementation.
	// For multi-line values (which single quotes cannot carry), use double
	// quotes AND additionally escape $ and backtick to prevent command
	// substitution.
	if !strings.ContainsAny(value, "\n\r") {
		return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
	}
	var out strings.Builder
	out.WriteByte('"')
	for _, r := range value {
		switch r {
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		case '\\':
			out.WriteString(`\\`)
		case '"':
			out.WriteString(`\"`)
		case '$':
			out.WriteString(`\$`)
		case '`':
			out.WriteString("\\`")
		default:
			out.WriteRune(r)
		}
	}
	out.WriteByte('"')
	return out.String()
}

func writeHydratedEnvFile(path string, content []byte, force bool) error {
	if err := ensureHydrateTargetWritable(path, force); err != nil {
		return err
	}
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create env target parent: %w", err)
	}
	tmp, err := os.CreateTemp(parent, ".devstrap-env-*.tmp")
	if err != nil {
		return fmt.Errorf("create env temp file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("secure env temp file: %w", err)
	}
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write env temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close env temp file: %w", err)
	}
	if force {
		if err := os.Rename(tmpPath, path); err != nil {
			return fmt.Errorf("install env file: %w", err)
		}
		cleanup = false
	} else {
		if err := os.Link(tmpPath, path); err != nil {
			if errors.Is(err, os.ErrExist) {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("refusing to overwrite existing env file %s; pass --force", path)}
			}
			return fmt.Errorf("install env file: %w", err)
		}
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secure env file: %w", err)
	}
	return nil
}

func ensureHydrateTargetWritable(path string, force bool) error {
	if force {
		return nil
	}
	if _, err := os.Stat(path); err == nil {
		return appError{code: exitInvalidConfig, err: fmt.Errorf("refusing to overwrite existing env file %s; pass --force", path)}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat env target: %w", err)
	}
	return nil
}

func ensureIgnored(projectPath, envPath string) error {
	rel, err := filepath.Rel(projectPath, envPath)
	if err != nil || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." || filepath.IsAbs(rel) {
		return nil
	}
	rel = filepath.ToSlash(rel)
	ignorePath := filepath.Join(projectPath, ".gitignore")
	//nolint:gosec // The path is the project's own .gitignore after confirming envPath is inside projectPath.
	raw, err := os.ReadFile(ignorePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read .gitignore: %w", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == rel {
			return nil
		}
	}
	var out bytes.Buffer
	out.Write(raw)
	if len(raw) > 0 && !bytes.HasSuffix(raw, []byte("\n")) {
		out.WriteByte('\n')
	}
	out.WriteString(rel)
	out.WriteByte('\n')
	//nolint:gosec // .gitignore is a normal project file and should remain readable with the repository.
	if err := os.WriteFile(ignorePath, out.Bytes(), 0o644); err != nil {
		return fmt.Errorf("update .gitignore: %w", err)
	}
	return nil
}
