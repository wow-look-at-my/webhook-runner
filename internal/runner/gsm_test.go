package runner

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The github-state-mirror routing injection: UNCONDITIONAL for every hook,
// manager, and test container, with no off switch and no per-id exemption.
func TestGSMInjectArgs(t *testing.T) {
	want := []string{"-e", "GITHUB_API_URL=https://github-state-mirror.pazer.io"}
	assert.Equal(t, want, gsmInjectArgs(), "every container is routed at the mirror")
	assert.Equal(t, "https://github-state-mirror.pazer.io", GSMBaseURL)

	// THE MIRROR IS A PROXY, NOT A FIREWALL. An `--add-host
	// api.github.com:0.0.0.0` must never appear here: a blackhole breaks the
	// callers that cannot honor GITHUB_API_URL (tenant CI job steps) instead
	// of routing them.
	for _, arg := range gsmInjectArgs() {
		assert.NotContains(t, arg, "0.0.0.0", "no blackhole")
		assert.NotEqual(t, "--add-host", arg, "no hosts-file override")
	}
}
