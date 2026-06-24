package cli

import (
	"fmt"
	"io"
	"os/exec"

	"github.com/Reederey87/DevStrap/internal/config"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func newDoctorCommand(stdout io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check local prerequisites",
		RunE: func(cmd *cobra.Command, args []string) error {
			paths := config.Paths{Home: viper.GetString("home"), Root: viper.GetString("root")}
			_, _ = fmt.Fprintf(stdout, "DevStrap home: %s\n", paths.Home)
			_, _ = fmt.Fprintf(stdout, "Managed root: %s\n", paths.Root)
			for _, tool := range []string{"git", "gh", "go"} {
				path, err := exec.LookPath(tool)
				if err != nil {
					_, _ = fmt.Fprintf(stdout, "%s: missing\n", tool)
					continue
				}
				_, _ = fmt.Fprintf(stdout, "%s: %s\n", tool, path)
			}
			return nil
		},
	}
}
