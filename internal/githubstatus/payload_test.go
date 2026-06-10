package githubstatus

import (
	"github.com/stretchr/testify/assert"
	"testing"
)

func TestParseRepoSHA(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantRepo string
		wantSHA  string
	}{
		{
			"push", `{"after":"deadbeef","repository":{"full_name":"o/r"}}`,
			"o/r", "deadbeef",
		},
		{
			"pull_request", `{"pull_request":{"head":{"sha":"abc"}},"repository":{"full_name":"o/r"}}`,
			"o/r", "abc",
		},
		{
			"check_suite", `{"check_suite":{"head_sha":"feed"},"repository":{"full_name":"o/r"}}`,
			"o/r", "feed",
		},
		{
			"head_commit fallback", `{"head_commit":{"id":"hcid"},"repository":{"full_name":"o/r"}}`,
			"o/r", "hcid",
		},
		{
			"after wins over pull_request",
			`{"after":"a","pull_request":{"head":{"sha":"b"}},"repository":{"full_name":"o/r"}}`,
			"o/r", "a",
		},
		{
			"branch deletion (zero SHA) falls through",
			`{"after":"0000000000000000000000000000000000000000","head_commit":{"id":"hcid"},"repository":{"full_name":"o/r"}}`,
			"o/r", "hcid",
		},
		{
			"missing repo",
			`{"after":"x"}`, "", "x",
		},
		{
			"invalid json",
			`not json`, "", "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, s := ParseRepoSHA([]byte(tc.body))
			assert.False(t, r != tc.wantRepo || s != tc.wantSHA)

		})
	}
}
