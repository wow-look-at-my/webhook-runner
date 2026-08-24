#!/bin/sh
# Docker-in-Docker smoke check. This runs inside the hook's --privileged
# container (the runner adds --privileged + an anonymous /var/lib/docker
# volume when dind:true). It starts a NESTED dockerd — whose storage lives
# on that volume because an inner overlay driver can't stack on the outer
# container's overlay rootfs — and proves it works via `docker version` &&
# `docker info`. Loud non-zero exit if the daemon never comes up. The same
# script is the container CMD (live run) and the declared test command, so
# both the run and `webhook-runner test` paths exercise the capability.
set -eu

# Talk to the daemon over the local unix socket only; no TLS/TCP needed for
# a local smoke check.
export DOCKER_TLS_CERTDIR=""

# cgroup v2 nesting: PID 1 here is this shell, not dockerd, so the nested
# daemon can't enable subtree controllers until we move our own processes
# into a leaf cgroup first (moby/moby hack/dind). Bounded + best-effort:
# a no-op on cgroup v1, and if it can't converge we still try to start the
# daemon (the readiness poll below is the real gate). --make-rshared keeps
# mount propagation working for nested containers.
mount --make-rshared / 2>/dev/null || :
if [ -f /sys/fs/cgroup/cgroup.controllers ]; then
	mkdir -p /sys/fs/cgroup/init
	n=0
	while [ "$n" -lt 10 ]; do
		xargs -rn1 </sys/fs/cgroup/cgroup.procs >/sys/fs/cgroup/init/cgroup.procs 2>/dev/null || :
		if sed -e 's/ / +/g' -e 's/^/+/' </sys/fs/cgroup/cgroup.controllers \
			>/sys/fs/cgroup/cgroup.subtree_control 2>/dev/null; then
			break
		fi
		n=$((n + 1))
		sleep 1
	done
fi

# Start the daemon via the base image's own entrypoint (handles the
# iptables legacy/nft selection); background it — this script is the
# workload that then drives the daemon.
dockerd-entrypoint.sh dockerd >/tmp/dockerd.log 2>&1 &

# Poll the daemon (via /var/run/docker.sock) with flat 1s sleeps, up to ~60.
i=0
until docker version >/dev/null 2>&1; do
	i=$((i + 1))
	if [ "$i" -ge 60 ]; then
		echo "dind-smoke: nested dockerd did not become ready within 60s" >&2
		tail -n 40 /tmp/dockerd.log 2>/dev/null >&2 || :
		exit 1
	fi
	sleep 1
done

docker version
docker info
echo "dind-smoke-ok"
