package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/wow-look-at-my/webhook-runner/internal/concurrency"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/runner"
)

func init() {
	rootCmd.AddCommand(&cobra.Command{
		Use:   "validate <hooks-dir>",
		Short: "Validate every hook.json in the given directory",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// One detection rule everywhere: serve, validate, and test all
			// resolve the layout the same way (see internal/hooks/layout.go).
			layout := hooks.DetectLayout(args[0])
			fmt.Fprintf(cmd.OutOrStdout(), "layout: %s\n", layout)
			loaded, errs := hooks.LoadLayout(layout)

			// Load the central concurrency groups and verify every hook's
			// concurrency_group is declared there — referencing an
			// undeclared group is a validation failure.
			cfg, cerr := concurrency.LoadFile(layout.ConcurrencyPath())
			if cerr != nil {
				errs = append(errs, cerr)
				cfg = &concurrency.Config{Groups: map[string]concurrency.Group{}}
			}
			refs := make(map[string]string, len(loaded))
			for id, h := range loaded {
				refs[id] = h.ConcurrencyGroup
			}
			badRef := map[string]bool{}
			for _, re := range concurrency.CheckRefs(cfg, refs) {
				errs = append(errs, re)
				badRef[re.HookID] = true
			}

			out := cmd.OutOrStdout()
			for _, name := range cfg.Names() {
				fmt.Fprintf(out, "group %s (limit %d)\n", name, cfg.Limit(name))
			}
			for id, h := range loaded {
				if badRef[id] {
					continue // surfaced as an error below
				}
				tag, tagErr := runner.ImageTag(h)
				if tagErr != nil {
					tag = "?"
				}
				grp := ""
				if h.ConcurrencyGroup != "" {
					grp = " [group: " + h.ConcurrencyGroup + "]"
				}
				fmt.Fprintf(out, "ok  %s (%s)%s\n", id, tag, grp)
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
