package runner

import (
	"fmt"
	"os"

	"github.com/wow-look-at-my/webhook-runner/internal/runs"

	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
)

// resolveSettingsFile rewrites the run's mounted settings document with its
// ${env:NAME} references resolved -- secrets first, then the runner host's
// environment, the same resolution api_key and the superseded env block use.
//
// It runs after the secrets load and before the container is launched,
// because that is the first moment both inputs exist: `${settings:...}`
// references were already resolved at LOAD (the schema validated the result),
// but a host variable cannot be read at validation time and must not be.
//
// An unresolvable reference FAILS THE RUN. It is never replaced with an empty
// string: a hook that starts with a blank credential looks healthy, does the
// wrong thing, and fails somewhere downstream -- the exact failure mode
// settings exist to end.
func resolveSettingsFile(hook *hooks.Hook, secrets map[string]string, path string) error {
	expanded, err := hooks.ExpandSettingsEnvRefs(hook.SettingsJSON(), hooks.SecretsFirstLookup(secrets))
	if err != nil {
		return fmt.Errorf("settings: %w", err)
	}
	if err := os.WriteFile(path, expanded, 0o600); err != nil {
		return fmt.Errorf("write settings: %w", err)
	}
	return nil
}

// supersededEnvArgs renders the `env` block into docker -e flags. It is still
// injected exactly as it always was: a deprecation that quietly stops working
// is worse than the flag day it exists to avoid -- the hook would load, run,
// and behave wrongly. Values resolve ${NAME} from the entity's secrets first,
// then the host environment.
//
// Every entity still using this is named at load (hooks.Deprecations) and
// listed on the needs-attention surface; the field goes away once the fleet
// has migrated to settings.
func (r *Runner) supersededEnvArgs(hook *hooks.Hook, run *runs.Run, secrets map[string]string) []string {
	if len(hook.Env) == 0 {
		return nil
	}
	lookup := hooks.SecretsFirstLookup(secrets)
	args := make([]string, 0, 2*len(hook.Env))
	for k, v := range hook.Env {
		expanded, missing := hooks.ExpandEnvRefs(v, lookup)
		for _, name := range missing {
			r.log.Warn("hook env references unset variable",
				"hook", hook.ID, "run", run.ID(), "env", k, "var", name)
			// A hook running with an empty secret looks healthy from the
			// outside while every run fails downstream.
			r.events.Record("env.unresolved",
				hook.ID+": env "+k+" references unset ${"+name+"}; the container gets an empty value",
				map[string]string{"hook": hook.ID, "run": run.ID()})
		}
		args = append(args, "-e", k+"="+expanded)
	}
	return args
}
