package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Reederey87/DevStrap/internal/childenv"
	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/envbundle"
	"github.com/Reederey87/DevStrap/internal/envfile"
	"github.com/Reederey87/DevStrap/internal/platform"
	"github.com/Reederey87/DevStrap/internal/state"
	"github.com/spf13/cobra"
)

func newRunCommand(stdout io.Writer, opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "run <path> -- <command> [args...]",
		Short: "Run a command with the project env profile",
		Args:  cobra.MinimumNArgs(2),
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
			localPath := project.LocalPath
			if localPath == "" {
				localPath = filepath.Join(opts.paths().Root, filepath.FromSlash(project.Path))
			}
			profile, bindings, err := store.EnvProfileForProject(cmd.Context(), project.ID)
			if err != nil {
				return err
			}
			device, err := store.CurrentDevice(cmd.Context())
			if err != nil {
				return err
			}
			env, runArgs, cleanup, err := runtimeEnvCommand(cmd.Context(), opts, device.ID, profile, bindings, args[1:])
			if cleanup != nil {
				defer cleanup()
			}
			if err != nil {
				return err
			}
			if err := runChildCommand(cmd.Context(), localPath, env, runArgs, stdout, cmd.ErrOrStderr()); err != nil {
				return err
			}
			return nil
		},
	}
}

func runtimeEnvCommand(ctx context.Context, opts *options, deviceID string, profile state.EnvProfile, bindings []state.SecretBinding, command []string) ([]string, []string, func(), error) {
	switch profile.Provider {
	case "devstrap_encrypted":
		set, err := decryptEnvProfile(ctx, opts, deviceID, bindings)
		if err != nil {
			return nil, nil, nil, err
		}
		env, err := childenv.FromOS(childenv.BasicAllowlist(), set)
		return env, command, nil, err
	case "1password":
		refsFile, cleanup, err := writeProviderRefsFile(bindings)
		if err != nil {
			return nil, nil, nil, err
		}
		env, err := childenv.FromOS(append(childenv.BasicAllowlist(), "OP_*"), nil)
		if err != nil {
			cleanup()
			return nil, nil, nil, err
		}
		runArgs := append([]string{"op", "run", "--env-file", refsFile, "--"}, command...)
		return env, runArgs, cleanup, nil
	default:
		return nil, nil, nil, appError{code: exitInvalidConfig, err: fmt.Errorf("env profile %s uses unsupported provider %s", profile.Name, profile.Provider)}
	}
}

func decryptEnvProfile(ctx context.Context, opts *options, deviceID string, bindings []state.SecretBinding) (map[string]string, error) {
	ref, err := sharedEncryptedRef(bindings)
	if err != nil {
		return nil, err
	}
	ciphertext, err := readEnvBlob(opts.paths(), ref)
	if err != nil {
		return nil, err
	}
	identity, err := devicekeys.NewHybridStore(opts.paths().KeyDir(), platform.Detect().Keychain).Read(ctx, deviceID)
	if err != nil {
		return nil, fmt.Errorf("read local device identity: %w", err)
	}
	plaintext, err := envbundle.Decrypt(ciphertext, identity.Private)
	if err != nil {
		return nil, err
	}
	set := make(map[string]string, len(plaintext.Vars))
	for _, binding := range plaintext.Vars {
		set[binding.Name] = binding.Value
	}
	return set, nil
}

func writeProviderRefsFile(bindings []state.SecretBinding) (string, func(), error) {
	if len(bindings) == 0 {
		return "", nil, fmt.Errorf("env profile has no bindings")
	}
	var refs []envfile.Binding
	for _, binding := range bindings {
		if binding.ProviderRef == "" {
			return "", nil, fmt.Errorf("provider ref for %s is empty", binding.VarName)
		}
		refs = append(refs, envfile.Binding{Name: binding.VarName, Value: binding.ProviderRef})
	}
	file, err := os.CreateTemp("", "devstrap-op-refs-*.env")
	if err != nil {
		return "", nil, fmt.Errorf("create provider refs file: %w", err)
	}
	path := file.Name()
	cleanup := func() { _ = os.Remove(path) }
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		cleanup()
		return "", nil, fmt.Errorf("secure provider refs file: %w", err)
	}
	if _, err := file.Write(renderDotenv(refs)); err != nil {
		_ = file.Close()
		cleanup()
		return "", nil, fmt.Errorf("write provider refs file: %w", err)
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("close provider refs file: %w", err)
	}
	return path, cleanup, nil
}

func injectProviderRefs(ctx context.Context, refsFile string) ([]byte, error) {
	out, err := os.CreateTemp("", "devstrap-op-inject-*.env")
	if err != nil {
		return nil, fmt.Errorf("create provider inject output: %w", err)
	}
	outPath := out.Name()
	if err := out.Chmod(0o600); err != nil {
		_ = out.Close()
		_ = os.Remove(outPath)
		return nil, fmt.Errorf("secure provider inject output: %w", err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(outPath)
		return nil, fmt.Errorf("close provider inject output: %w", err)
	}
	defer func() { _ = os.Remove(outPath) }()

	env, err := childenv.FromOS(append(childenv.BasicAllowlist(), "OP_*"), nil)
	if err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, "op", "inject", "--in-file", refsFile, "--out-file", outPath, "--file-mode", "0600", "--force") //nolint:gosec // fixed 1Password CLI command with controlled args and sanitized env.
	command.Env = env
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("op inject failed: %w", err)
	}
	raw, err := os.ReadFile(outPath) //nolint:gosec // outPath is a just-created 0600 temporary provider output file.
	if err != nil {
		return nil, fmt.Errorf("read provider inject output: %w", err)
	}
	return raw, nil
}

func runChildCommand(ctx context.Context, dir string, env []string, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return appError{code: exitInvalidConfig, err: fmt.Errorf("command is required")}
	}
	command := exec.CommandContext(ctx, args[0], args[1:]...) //nolint:gosec // command is the explicit user-selected process for `devstrap run`.
	command.Dir = dir
	command.Env = env
	command.Stdout = stdout
	command.Stderr = stderr
	command.Stdin = os.Stdin
	if err := command.Run(); err != nil {
		// CLI-03: propagate the child's real exit code for passthrough wrappers
		// so callers can distinguish test failures from transient errors.
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return appError{code: childExitBase + ee.ExitCode(), err: fmt.Errorf("run %s: command exited %d", strings.Join(args, " "), ee.ExitCode())}
		}
		return fmt.Errorf("run %s: %w", strings.Join(args, " "), err)
	}
	return nil
}
