#!/bin/sh
# What `dind: true` delivers, and what no container gets, checked against a real
# docker daemon.
#
# dind gives ONE thing: an anonymous volume at /var/lib/docker, because an inner
# daemon cannot stack its overlay driver on the outer container's overlay
# rootfs. It adds no privilege. So this proves the volume is there and writable,
# and then proves the host is still out of reach.
#
# The second half is the point. /proc/sysrq-trigger reboots the host and
# /proc/sys/kernel/core_pattern names a helper the HOST kernel runs as real root
# on any core dump. Both are refused only because docker binds those paths
# read-only, and exactly two flags remove that bind -- --privileged and
# systempaths=unconfined. Both have been in this runner. hostprimitives_test.go
# pins the argv; this pins the OUTCOME, against the daemon, where a docker
# release that changed its defaults would also show up.
#
# The same script is the container CMD and the declared test command, so the
# live-run path and `webhook-runner test` both prove it.
set -eu

# What dind is for.
if [ ! -d /var/lib/docker ]; then
	echo "dind-smoke: /var/lib/docker is missing -- the dind volume was not mounted" >&2
	exit 1
fi
if ! touch /var/lib/docker/.probe 2>/dev/null; then
	echo "dind-smoke: /var/lib/docker is not writable -- an inner daemon could not use it" >&2
	exit 1
fi
rm -f /var/lib/docker/.probe

# What no container may have. Each write MUST fail. A shell that can perform
# either of these owns the host, so a success here is the loudest possible
# failure of this test.
#
# core_pattern is re-written with the value already there, and sysrq-trigger is
# probed with a no-op level rather than a command, so a regression that lets the
# write through does not also reboot the CI runner. Each redirect runs in a
# SUBSHELL: a failed redirect is reported by the shell itself, not by the
# command, so 2>/dev/null only reaches it from outside -- otherwise a passing
# run prints three "cannot create" lines and reads like a failure.
existing_pattern=$(cat /proc/sys/kernel/core_pattern 2>/dev/null || echo core)
if ( printf '%s' "$existing_pattern" >/proc/sys/kernel/core_pattern ) 2>/dev/null; then
	echo "dind-smoke: WROTE /proc/sys/kernel/core_pattern -- this container can run code as root ON THE HOST" >&2
	exit 1
fi
if ( printf '0' >/proc/sysrq-trigger ) 2>/dev/null; then
	echo "dind-smoke: WROTE /proc/sysrq-trigger -- this container can reboot the host" >&2
	exit 1
fi

# /proc/sys as a whole, not just the two files above: the read-only bind covers
# the tree, and a partial one would leave a different global writable.
if ( printf '1' >/proc/sys/kernel/pid_max ) 2>/dev/null; then
	echo "dind-smoke: /proc/sys is writable -- docker's read-only bind is not in place" >&2
	exit 1
fi

echo "dind-storage-ok"
