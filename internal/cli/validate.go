package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
)

func init() {
	rootCmd.AddCommand(&cobra.Command{
		Use:   "validate <hooks-dir>",
		Short: "Validate every hook.json in the given directory",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			loaded, errs := hooks.LoadDir(args[0])
			out := cmd.OutOrStdout()
			for id, h := range loaded {
				fmt.Fprintf(out, "ok  %s (%s)\n", id, h.Image)
			}
			if len(errs) > 0 {
				for _, e := range errs {
					fmt.Fprintf(cmd.ErrOrStderr(), "ERR %v\n", e)
				}
				return errors.New("one or more hooks failed validation")
			}
			fmt.Fprintf(out, "%d hook(s) validated\n", len(loaded))
			return nil
		},
	})
}
