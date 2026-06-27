package platform

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/Reederey87/DevStrap/internal/childenv"
)

type SystemEditor struct{}

func (SystemEditor) Name() string { return "system-editor" }

func (SystemEditor) Open(ctx context.Context, dir, editor string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if _, err := exec.LookPath(editor); err != nil {
		return fmt.Errorf("%w: %s", ErrEditorNotFound, editor)
	}
	//nolint:gosec // The editor binary is user-selected, resolved with LookPath, and launched with a literal "--" separator.
	cmd := exec.Command(editor, "--", dir)
	env, err := childenv.FromOS(childenv.BasicAllowlist(), nil)
	if err != nil {
		return err
	}
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("open editor: %w", err)
	}
	if cmd.Process != nil {
		_ = cmd.Process.Release()
	}
	return nil
}
