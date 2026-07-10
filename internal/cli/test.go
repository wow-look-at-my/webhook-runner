package cli

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
	"github.com/wow-look-at-my/webhook-runner/internal/runner"
)

func init() {
	var (
		timeout time.Duration
		docker  string
		only    []string
	)
	cmd := &cobra.Command{
		Use:   "test <hooks-dir>",
		Short: "Run every hook's declared tests (hook.json \"tests\") in its image",
		Long: `Run the test commands hooks declare in their hook.json "tests" array.

Each test command runs in a fresh container of the hook's image. For hooks
that ship a Dockerfile the image is built first (the same content-hash tag
a live run uses), so tests exercise exactly the baked code — copy test
files into the image and set WORKDIR so relative paths like
"node --test x.test.ts" resolve. Tests get no payload, no hook env, and no
secrets: they must be self-contained.

Hooks without a "tests" array are skipped. Exits non-zero if any hook fails
to load or any test command fails.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			loaded, errs := hooks.LoadDir(args[0])
			if len(errs) > 0 {
				for _, e := range errs {
					fmt.Fprintf(cmd.ErrOrStderr(), "ERR %v\n", e)
				}
				return errors.New("one or more hooks failed validation")
			}
			ids := make([]string, 0, len(loaded))
			for id := range loaded {
				ids = append(ids, id)
			}
			sort.Strings(ids)

			for _, want := range only {
				if _, ok := loaded[want]; !ok {
					return fmt.Errorf("--hook %s: no such hook", want)
				}
			}

			out := cmd.OutOrStdout()
			tested, commands := 0, 0
			var failures []error
			for _, id := range ids {
				h := loaded[id]
				if len(h.Tests) == 0 {
					continue
				}
				if len(only) > 0 && !slices.Contains(only, id) {
					continue
				}
				tested++
				commands += len(h.Tests)
				if err := runner.RunHookTests(h, runner.TestOptions{
					Docker:  docker,
					Timeout: timeout,
					Out:     out,
				}); err != nil {
					failures = append(failures, err)
				}
			}
			if len(failures) > 0 {
				for _, e := range failures {
					fmt.Fprintf(cmd.ErrOrStderr(), "FAIL %v\n", e)
				}
				return fmt.Errorf("%d of %d hook(s) with tests failed", len(failures), tested)
			}
			if tested == 0 {
				fmt.Fprintln(out, "no hooks declare tests")
				return nil
			}
			fmt.Fprintf(out, "%d test command(s) passed across %d hook(s)\n", commands, tested)
			return nil
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", runner.DefaultTestTimeout, "per-test-command timeout")
	cmd.Flags().StringVar(&docker, "docker", "", `docker binary (default "docker")`)
	cmd.Flags().StringSliceVar(&only, "hook", nil, "only run tests for these hook IDs (repeatable)")
	rootCmd.AddCommand(cmd)
}
