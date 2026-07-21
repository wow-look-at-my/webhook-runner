package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/wow-look-at-my/webhook-runner/internal/concurrency"
	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/runner"
	"strings"
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
			// Managers validate alongside hooks: same Dockerfile/$schema/
			// skip_if/auth rules plus reconcile_interval, a hook/manager id
			// collision check (one id namespace), and the same
			// concurrency-group reference rule.
			loadedManagers, merrs := hooks.LoadManagers(layout)
			errs = append(errs, merrs...)
			for id := range loadedManagers {
				if _, clash := loaded[id]; clash {
					errs = append(errs, fmt.Errorf("manager %q: id collides with a hook of the same name (hooks and managers share one id namespace)", id))
					delete(loadedManagers, id)
				}
			}

			refs := make(map[string]string, len(loaded)+len(loadedManagers))
			for id, h := range loaded {
				refs[id] = h.ConcurrencyGroup
			}
			for id, m := range loadedManagers {
				refs[id] = m.ConcurrencyGroup
			}
			badRef := map[string]bool{}
			for _, re := range concurrency.CheckRefs(cfg, refs) {
				errs = append(errs, re)
				badRef[re.HookID] = true
			}
			// spawn_targets must name declared hooks — the manifest is the
			// spawn allowlist; an undeclared target fails validation.
			checkable := make(map[string]*hooks.Hook, len(loaded))
			for id, h := range loaded {
				if !badRef[id] {
					checkable[id] = h
				}
			}
			checkableManagers := make(map[string]*hooks.Manager, len(loadedManagers))
			for id, m := range loadedManagers {
				if !badRef[id] {
					checkableManagers[id] = m
				}
			}
			for _, se := range hooks.CheckSpawnTargets(checkable, checkableManagers) {
				errs = append(errs, se)
				badRef[se.ManagerID] = true
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
			for id, m := range loadedManagers {
				if badRef[id] {
					continue // surfaced as an error below
				}
				tag, tagErr := runner.ImageTag(m.Hook)
				if tagErr != nil {
					tag = "?"
				}
				iv := "event-only"
				if m.ReconcileIntervalRaw != "" {
					iv = "reconcile " + m.ReconcileIntervalRaw
				}
				spawns := ""
				if len(m.SpawnTargets) > 0 {
					spawns = " [spawns: " + strings.Join(m.SpawnTargets, ",") + "]"
				}
				fmt.Fprintf(out, "ok  %s (manager, %s, %s)%s\n", id, tag, iv, spawns)
			}
			if len(errs) > 0 {
				for _, e := range errs {
					fmt.Fprintf(cmd.ErrOrStderr(), "ERR %v\n", e)
				}
				return errors.New("one or more hooks failed validation")
			}
			if len(loadedManagers) > 0 {
				fmt.Fprintf(out, "%d hook(s) + %d manager(s) validated\n", len(loaded), len(loadedManagers))
			} else {
				fmt.Fprintf(out, "%d hook(s) validated\n", len(loaded))
			}
			return nil
		},
	})
}
