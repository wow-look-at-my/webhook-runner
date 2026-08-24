package runner

// The docker flags a `dind: true` entity gets, in ONE place so the live-run,
// `webhook-runner test` and manager-session paths cannot drift.
//
// --privileged is NOT among them, by operator ruling. A privileged container
// holds every capability, an unmasked /proc and full device access, which makes
// it host-root-equivalent: a writable /proc/sys/kernel/core_pattern alone is a
// global, non-namespaced file whose helper the HOST kernel runs as real root on
// any core dump, and a scratch mount already gives the container a writable host
// path to aim it at. A fleet that executes other people's CI must not hold that.
//
// The consequence is deliberate and loud: a nested daemon started as root
// (`dockerd`) will not come up without those capabilities, and the run fails
// saying so. An image that wants a nested daemon has to run a ROOTLESS one
// (dockerd-rootless.sh, which needs a user namespace, fuse-overlayfs or vfs
// storage, and no iptables). Nothing here fakes that for it -- an entity whose
// image still launches root dockerd fails every run until the image changes,
// which is the honest signal that the conversion has not happened.
//
// see docs/internals/hooks-images-and-reload.md

// dindStorageArgs returns the nested daemon's storage mount, or nothing when
// the entity's own `scratch` already covers that path -- two mounts on one
// destination is a docker error, so getting this wrong fails every dind run.
// The inner daemon needs a real filesystem there: its overlay driver cannot
// stack on the outer container's overlay rootfs.
func dindStorageArgs(covered bool) []string {
	if covered {
		return nil
	}
	return []string{"--mount", "type=volume,dst=" + dindStorageDir}
}
