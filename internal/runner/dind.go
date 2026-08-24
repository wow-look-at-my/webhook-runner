package runner

// What a `dind: true` entity gets, in ONE place so the live-run,
// `webhook-runner test` and manager-session paths cannot drift.
//
// It is a mount, and nothing else. --privileged is NOT among them, by operator
// ruling: a privileged container holds every capability, an unmasked /proc and
// full device access, which makes it host-root-equivalent. /proc/sys/kernel/
// core_pattern alone is a global, non-namespaced file whose helper the HOST
// kernel runs as real root on any core dump, and /proc/sysrq-trigger reboots
// the machine on one write. A fleet that executes other people's CI must not
// hold that, so no run gets it and there is no field that asks for it.
//
// The consequence is deliberate and loud. A nested daemon started as root
// cannot come up here: it needs to write /proc/sys and /sys/fs/cgroup, both
// read-only without --privileged, and the run fails saying so. Nothing fakes
// it. see docs/internals/nested-containers.md for what an unprivileged
// container can and cannot host, measured.

// dindStorageDir is where a nested daemon keeps its images and layers. It has
// to be a real filesystem: an overlay driver cannot stack on the outer
// container's overlay rootfs.
const dindStorageDir = "/var/lib/docker"

// dindArgs returns the docker flags for an entity's `dind` field. The volume is
// anonymous, so --rm reaps it when the run ends and inner storage never leaks
// between runs. The host's own daemon is never exposed.
func dindArgs(dind bool) []string {
	if !dind {
		return nil
	}
	return []string{"--mount", "type=volume,dst=" + dindStorageDir}
}
