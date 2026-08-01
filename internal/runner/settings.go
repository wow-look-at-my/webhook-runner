package runner

import (
	"fmt"
	"os"

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
