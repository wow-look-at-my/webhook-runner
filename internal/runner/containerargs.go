package runner

// The ONE place `docker run` argv is assembled, for every container start
// (hook run, manager instance, hook test). This is what makes GSM routing
// and PID-namespace isolation hold for all of them: nothing else builds argv.

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
	// label is the orphan-sweep marker for a live hook run's reap-on-boot.
	label string
	// entrypoint overrides the image's, for the paths that inject the KV shim.
	entrypoint string
	// mounts are -v values: payload/headers/settings files, the KV shim, a hook's volumes.
	mounts []string
	// env are -e values applied before the mirror injection and secrets below.
	env []string
	// secrets apply after env, sorted by key for reproducible argv; a hook's own env still wins.
	secrets map[string]string
	// onReservedSecret is called for a skipped, reserved-key secret. Nil means no logging.
	onReservedSecret func(key string)
	networks         []string
	user             string
	workdir          string
	// dind grants nested-dockerd privilege; it does not widen the PID namespace.
	dind bool
	// devices are --device host node passthroughs, an audited grant like dind.
	devices []string
	// seccomp is the rendered seccompArgs output; empty when the entity opts into nothing.
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
	// Unconditional: every container's GitHub traffic rides the mirror. No opt-out.
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
	// The anonymous /var/lib/docker volume gives the nested daemon storage on a
	// real filesystem -- its overlay driver cannot stack on the outer
	// container's overlay rootfs -- and --rm above reaps it at exit, so inner
	// storage never leaks between runs. The host's daemon is never exposed.
	args = append(args, dindArgs(s.dind)...)
	for _, d := range s.devices {
		args = append(args, "--device", d)
	}
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
