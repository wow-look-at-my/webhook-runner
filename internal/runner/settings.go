package runner

import (
	"fmt"
	"os"

	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
)

// resolveSettingsFile rewrites the run's mounted settings document with its ${env:NAME} references resolved -- secrets , then the runner host's environment, the same resolution api_key and the superseded env block use.
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
