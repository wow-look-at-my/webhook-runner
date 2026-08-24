#!/bin/sh
# The dind contract, checked from inside the container: the storage arrives AND
# the container can do what a nested daemon needs. This script is both the
# container CMD (live run) and the declared test command, so both paths prove
# the same thing.
#
# The capability half is the point. Dropping --privileged left the volume
# arriving exactly as before, so a check that only looked for the mount stayed
# green while every real dind run died at cgroup-prep.
set -eu

fail() {
	echo "dind-smoke: $1" >&2
	exit 1
}

# The anonymous volume. /var/lib/docker must be its own mount rather than part
# of the container's overlay rootfs: an inner overlay driver cannot stack on
# the outer one, so a nested daemon has nowhere to put layers without this.
grep -q " /var/lib/docker " /proc/self/mounts ||
	fail "/var/lib/docker is not a mount point -- the dind volume did not arrive"

# The three things the launcher's cgroup-prep does before it starts dockerd.
[ -w /proc/sys/kernel/core_pattern ] ||
	fail "/proc/sys is read-only -- dockerd cannot start; the container is not privileged"
mkdir -p /sys/fs/cgroup/init ||
	fail "/sys/fs/cgroup is read-only -- cgroup-v2 nesting prep cannot run"
mount --make-rshared / ||
	fail "mount --make-rshared failed -- the container lacks CAP_SYS_ADMIN"

echo "dind-smoke-ok: volume present, container can host a nested daemon"
