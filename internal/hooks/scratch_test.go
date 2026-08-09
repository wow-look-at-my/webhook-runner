package hooks

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const scratchSchemaLine = `"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json"`

func TestParseScratchAndTmpfs(t *testing.T) {
	h, err := parseInDir(t, `{`+scratchSchemaLine+`,"scratch":["/home/runner/_work","/var/lib/docker"],"tmpfs":["/tmp"],"read_only_rootfs":true}`)
	require.Nil(t, err)
	assert.Equal(t, []string{"/home/runner/_work", "/var/lib/docker"}, h.Scratch)
	assert.Equal(t, []string{"/tmp"}, h.Tmpfs)
	assert.True(t, h.ReadOnlyRootfs)

	h, err = parseInDir(t, `{`+scratchSchemaLine+`}`)
	require.Nil(t, err)
	assert.Empty(t, h.Scratch)
	assert.Empty(t, h.Tmpfs)
	assert.False(t, h.ReadOnlyRootfs, "read_only_rootfs defaults to false when omitted")
}

// Every rejection below would otherwise surface as a docker error at RUN time,
// on a fleet that already accepted the manifest. Catching them at load keeps a
// broken mount declaration out of a deployed tree entirely.
func TestScratchRejectsUnmountablePaths(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{
			name: "relative path",
			doc:  `"scratch":["home/runner/_work"]`,
			want: "must be an absolute container path",
		},
		{
			name: "unclean path",
			doc:  `"scratch":["/home/runner/../runner/_work"]`,
			want: "must be a clean path",
		},
		{
			name: "root",
			doc:  `"scratch":["/"]`,
			want: `must not be "/"`,
		},
		{
			name: "duplicate within scratch",
			doc:  `"scratch":["/tmp","/tmp"]`,
			want: "already mounted by scratch",
		},
		{
			name: "same path in scratch and tmpfs",
			doc:  `"scratch":["/tmp"],"tmpfs":["/tmp"]`,
			want: "already mounted by scratch",
		},
		{
			name: "relative tmpfs path",
			doc:  `"tmpfs":["tmp"]`,
			want: "must be an absolute container path",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseInDir(t, `{`+scratchSchemaLine+`,`+tc.doc+`}`)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestScratchCovers(t *testing.T) {
	h := &Hook{Scratch: []string{"/var/lib/docker", "/home/runner/_work"}}
	assert.True(t, h.ScratchCovers("/var/lib/docker"))
	assert.False(t, h.ScratchCovers("/var/lib/dockerx"))
	assert.False(t, (&Hook{}).ScratchCovers("/var/lib/docker"))
}
