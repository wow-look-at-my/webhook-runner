#!/bin/sh
# The dind contract, checked from inside the container: the storage arrives and
# the privilege does not. This script is both the container CMD (live run) and
# the declared test command, so both paths prove the same thing.
#
# The negative half is the point. `dind` used to mean --privileged, the flag is
# banned now (internal/runner/dind.go), and a check that only looked for the
# volume would keep passing if it came back.
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

# No privilege. Each of these is writable in a --privileged container, is
# read-only here, and is something a root dockerd needs.
if [ -w /proc/sys/kernel/core_pattern ]; then
	fail "/proc/sys is writable -- this container is privileged"
fi
if mkdir -p /sys/fs/cgroup/init 2>/dev/null; then
	fail "/sys/fs/cgroup is writable -- this container is privileged"
fi

# The capability is gone, not merely unused: the cgroup-v2 nesting prep that
# every root dockerd needs must fail here.
if mount --make-rshared / 2>/dev/null; then
	fail "mount --make-rshared succeeded -- this container holds CAP_SYS_ADMIN"
fi

echo "dind-smoke-ok: volume present, container unprivileged"
