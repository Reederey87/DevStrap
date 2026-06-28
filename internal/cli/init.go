package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Reederey87/DevStrap/internal/config"
	"github.com/Reederey87/DevStrap/internal/devicekeys"
	"github.com/Reederey87/DevStrap/internal/platform"
	"github.com/Reederey87/DevStrap/internal/state"
	"github.com/spf13/cobra"
)

func newInitCommand(stdout io.Writer, opts *options) *cobra.Command {
	var workspaceName string
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "init [root]",
		Short: "Initialize a DevStrap workspace",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			paths := opts.paths()
			if len(args) == 1 {
				if cmd.Root().PersistentFlags().Changed("root") {
					return appError{code: exitInvalidConfig, err: errors.New("use either positional root or --root, not both")}
				}
				paths.Root = args[0]
			}
			if workspaceName == "" {
				workspaceName = "default"
			}
			cleanRoot, err := cleanAbsPath(paths.Root)
			if err != nil {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("resolve root: %w", err)}
			}
			cleanHome, err := cleanAbsPath(paths.Home)
			if err != nil {
				return appError{code: exitInvalidConfig, err: fmt.Errorf("resolve home: %w", err)}
			}
			paths = config.Paths{Home: cleanHome, Root: cleanRoot}

			if dryRun {
				_, err := fmt.Fprintf(stdout, "Would create %s, %s, %s, and %s\n", paths.Root, paths.Home, paths.LogDir(), paths.StateDB())
				return err
			}

			//nolint:gosec // The managed code root is user-facing project storage, not a secret state directory.
			if err := os.MkdirAll(paths.Root, 0o755); err != nil {
				return fmt.Errorf("create root: %w", err)
			}
			if err := os.MkdirAll(paths.Home, 0o700); err != nil {
				return fmt.Errorf("create state home: %w", err)
			}
			if err := os.MkdirAll(paths.LogDir(), 0o700); err != nil {
				return fmt.Errorf("create log dir: %w", err)
			}
			if err := writeDefaultConfig(paths, workspaceName); err != nil {
				return err
			}

			store, err := state.Open(cmd.Context(), paths.StateDB())
			if err != nil {
				return err
			}
			defer closeStore(store)

			if err := store.Migrate(); err != nil {
				return err
			}
			if err := store.EnsureWorkspace(cmd.Context(), workspaceName, paths.Root); err != nil {
				return err
			}
			device, err := store.EnsureDevice(cmd.Context(), "")
			if err != nil {
				return err
			}
			if err := ensureLocalDeviceIdentity(cmd.Context(), paths, store, device); err != nil {
				return err
			}

			_, err = fmt.Fprintf(stdout, "Initialized DevStrap workspace %q at %s\n", workspaceName, paths.Root)
			return err
		},
	}

	cmd.Flags().StringVar(&workspaceName, "workspace-name", "", "workspace name")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show planned changes without writing")
	return cmd
}

func ensureLocalDeviceIdentity(ctx context.Context, paths config.Paths, store *state.Store, device state.Device) error {
	keyStore := devicekeys.NewHybridStore(paths.KeyDir(), platform.Detect().Keychain)
	if device.PublicKey != "" {
		identity, err := keyStore.Read(ctx, device.ID)
		if err != nil {
			return fmt.Errorf("read local device identity: %w", err)
		}
		if identity.Recipient != device.PublicKey {
			return fmt.Errorf("local device identity does not match stored public key")
		}
	} else {
		identity, _, err := keyStore.Ensure(ctx, device.ID)
		if err != nil {
			return fmt.Errorf("ensure local device identity: %w", err)
		}
		if err := store.SetDevicePublicKey(ctx, device.ID, identity.Recipient); err != nil {
			return err
		}
	}
	signingIdentity, _, err := keyStore.EnsureSigning(ctx, device.ID)
	if err != nil {
		return fmt.Errorf("ensure local device signing identity: %w", err)
	}
	if device.SigningPublicKey != "" && signingIdentity.Public != device.SigningPublicKey {
		return fmt.Errorf("local device signing identity does not match stored signing public key")
	}
	if device.SigningPublicKey == "" {
		if err := store.SetDeviceSigningPublicKey(ctx, device.ID, signingIdentity.Public); err != nil {
			return err
		}
	}
	return nil
}

func cleanAbsPath(path string) (string, error) {
	if path == "" {
		return "", errors.New("path must not be empty")
	}
	return filepath.Abs(filepath.Clean(path))
}

func writeDefaultConfig(paths config.Paths, workspaceName string) error {
	path := filepath.Join(paths.Home, "config.yaml")
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat config: %w", err)
	}
	content := fmt.Sprintf("home: %q\nroot: %q\nworkspace_name: %q\n", paths.Home, paths.Root, workspaceName)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}
