package runner

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The enforced-GitHub-gateway injection: INERT with the knob unset (the
// shipped default — the non-breaking proof's load-bearing assertion),
// blackhole + env default when set, and the operator's exemption list
// keeps named ids fully direct.
func TestGSMInjectArgs(t *testing.T) {
	// Knob unset: ZERO args for everyone — behaviorally identical to master.
	assert.Nil(t, gsmInjectArgs(GSMConfig{}, "any-hook"))
	assert.Nil(t, gsmInjectArgs(GSMConfig{Direct: map[string]bool{"x": true}}, "x"))

	cfg := GSMConfig{
		URL:    "https://github-state-mirror.pazer.io",
		Direct: map[string]bool{"gha-runner": true, "gha-runner-dind": true},
	}
	assert.Equal(t, []string{
		"--add-host", "api.github.com:0.0.0.0",
		"-e", "GITHUB_API_URL=https://github-state-mirror.pazer.io",
	}, gsmInjectArgs(cfg, "pr-minder"))

	// Exempt ids (the CI-runner fleets) stay direct: no blackhole, no env.
	assert.Nil(t, gsmInjectArgs(cfg, "gha-runner"))
	assert.Nil(t, gsmInjectArgs(cfg, "gha-runner-dind"))
}
