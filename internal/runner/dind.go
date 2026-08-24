// What a `dind: true` entity gets, in ONE place so the live-run,
// `webhook-runner test` and manager-session paths cannot drift.
//
// A nested daemon must write /proc/sys and /sys/fs/cgroup, and on this host
// --privileged is the only thing that makes them writable. Withdrawing it
// without a replacement took the wow-dind fleet down: every run died at
// cgroup-prep. docs/internals/nested-containers.md measures what an
// unprivileged container can host, and names what would replace this.
package runner

// dindStorageDir is a real filesystem: an overlay driver cannot stack on the outer container's overlay rootfs.
const dindStorageDir = "/var/lib/docker"

// dindArgs returns the docker flags for an entity's `dind` field.
func dindArgs(dind bool) []string {
	if !dind {
		return nil
	}
	return []string{"--privileged", "--mount", "type=volume,dst=" + dindStorageDir}
}
