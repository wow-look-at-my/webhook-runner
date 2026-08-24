package runner

// The ONE place `docker run` arguments are assembled.
//
// There used to be three: a live hook run (runner.go), a manager instance
// (managersession.go), and a hook's declared tests (tests.go). They agreed on
// what a container gets by being edited together, which is not agreement --
// each was free to drift, and a property that has to hold for ALL of them had
// to be re-checked in three places or hold by luck. Two such properties:
//
//   - github-state-mirror routing reaches every container, no exemptions.
//   - No container joins another PID namespace. Docker's default is a fresh
//     one, and a whole isolation property downstream rests on it: dats binds
//     the container's /proc read-only when the kernel refuses it a private
//     procfs, which is safe exactly because that procfs lists the container's
//     processes and nothing else. One --pid=host and hook code is reading the
//     host's process table, with nothing failing to say so.
//
// Both now hold by construction: the first because this builder injects it,
// the second because there is no field to ask for it and one function to read.

import (
	"sort"

	"github.com/wow-look-at-my/webhook-runner/internal/hooks"
)

// containerSpec is one container start, as data. Every field is what the CALLER
// chose; nothing is derived from a hook here, so a path that deliberately omits
// something -- a test container takes no secrets and no payload -- simply
// leaves it unset instead of needing a flag to suppress it.
//
// There is deliberately no namespace field of any kind. See the file comment.
type containerSpec struct {
	name  string
	image string
	// label is the orphan-sweep marker, set only where a later serve boot has
	// to be able to find and reap the container (a live hook run).
	label string
	// entrypoint overrides the image's, for the paths that inject the KV shim.
	entrypoint string
	// mounts are -v values in the caller's order: the payload/headers/settings
	// files, the KV shim and socket, and a hook's declared volumes.
	mounts []string
	// env are -e KEY=VALUE values in the caller's order, applied BEFORE the
	// mirror injection and the secrets below.
	env []string
	// secrets are injected after env and sorted by key, so one run's argv is
	// reproducible instead of depending on map iteration. Docker keeps the last
	// -e for a key, so a hook's own env still wins over a secret of that name.
	secrets map[string]string
	// onReservedSecret is called for a secret shadowing a reserved key, which
	// is skipped. Nil means the caller does not log them.
	onReservedSecret func(key string)
	networks         []string
	user             string
	workdir          string
	// dind gives the container storage a nested dockerd can use. It grants no
	// privilege at all -- see dind.go.
	dind bool
	// seccomp is whatever seccompArgs produced for this entity, already
	// rendered. Empty for an entity that opted into nothing.
	seccomp []string
	// argv trails the image: the command the container runs.
	argv []string
}

// args renders the spec as the full `docker run` argv.
//
// The order is fixed here rather than at each call site: docker does not care
// how -v and -e interleave, but it does care that everything precedes the
// image, and a reader comparing two container starts should be comparing the
// specs, not two hand-built slices.
func (s containerSpec) args() []string {
	args := []string{"run", "--rm", "--name", s.name}
	if s.label != "" {
		args = append(args, "--label", s.label)
	}
	if s.entrypoint != "" {
		args = append(args, "--entrypoint", s.entrypoint)
	}
	for _, m := range s.mounts {
		args = append(args, "-v", m)
	}
	for _, e := range s.env {
		args = append(args, "-e", e)
	}
	// Unconditional, and unconditional HERE so it cannot be forgotten by a
	// fourth caller: every container's GitHub traffic rides the mirror. It
	// precedes the secrets and a hook's own env, both of which may override it.
	args = append(args, gsmInjectArgs()...)
	for _, n := range s.networks {
		args = append(args, "--network", n)
	}
	for _, k := range sortedSecretKeys(s.secrets) {
		if hooks.ReservedEnvKey(k) {
			if s.onReservedSecret != nil {
				s.onReservedSecret(k)
			}
			continue
		}
		args = append(args, "-e", k+"="+s.secrets[k])
	}
	if s.user != "" {
		args = append(args, "--user", s.user)
	}
	if s.workdir != "" {
		args = append(args, "--workdir", s.workdir)
	}
	args = append(args, dindArgs(s.dind)...)
	args = append(args, s.seccomp...)
	args = append(args, s.image)
	return append(args, s.argv...)
}

func sortedSecretKeys(secrets map[string]string) []string {
	keys := make([]string, 0, len(secrets))
	for k := range secrets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
