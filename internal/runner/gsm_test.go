package runner

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The github-state-mirror routing injection: UNCONDITIONAL for every hook,
// manager, and test container (operator ruling -- — "*Everything*
// must go through GSM"), with no off switch and no per-id exemption.
func TestGSMInjectArgs(t *testing.T) {
	want := []string{"-e", "GITHUB_API_URL=https://github-state-mirror.pazer.io"}
	assert.Equal(t, want, gsmInjectArgs(), "every container is routed at the mirror")
	assert.Equal(t, "https://github-state-mirror.pazer.io", GSMBaseURL)

	// GSM IS A PROXY, NOT A FIREWALL (operator correction -- — "GSM is not a blackhole"): webhook-runner#'s `--add-host api.github.com:...` was.
	for _, arg := range gsmInjectArgs() {
		assert.NotContains(t, arg, "0.0.0.0", "no blackhole")
		assert.NotEqual(t, "--add-host", arg, "no hosts-file override")
	}
}
