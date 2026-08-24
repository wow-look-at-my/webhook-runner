#!/usr/bin/env bash
# Wires an ALREADY-EXISTING pair of datasets into docker: merges daemon.json,
# installs the systemd mount-ordering override, restarts dockerd, and
# verifies the result. Root, on the actual runner host -- nothing here can
# run from a build or CI environment.
#
# This script never runs zpool/zfs create, set, or destroy -- the operator
# owns the pool and its datasets, exactly as configured, and this script
# only ever READS them (zfs list) to check they are what docker is about to
# be pointed at. See require_dataset below and README.md's "Install" section
# for the two `zfs create` lines the operator runs by hand, once, first.
#
# Usage: bootstrap.sh <pool> [mount-root]
#   pool        an existing ZFS pool name (e.g. "tank"), with <pool>/docker
#               and <pool>/runners already created (see README.md)
#   mount-root  defaults to /mnt/pool -- must match the datasets' own
#               mountpoint property, which this script does not set
#
# Idempotent: re-running after a partial or failed run is safe. Read
# deploy/pool-storage/README.md first -- this script is the copy-paste
# tail of that document, not a replacement for understanding it.
set -euo pipefail

POOL="${1:?usage: bootstrap.sh <pool> [mount-root]}"
MOUNT_ROOT="${2:-/mnt/pool}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if [ "$(id -u)" -ne 0 ]; then
	echo "bootstrap.sh: must run as root on the runner host" >&2
	exit 1
fi
if ! command -v zfs > /dev/null; then
	echo "bootstrap.sh: no zfs binary -- this is not a ZFS host" >&2
	exit 1
fi

DOCKER_MOUNT="$MOUNT_ROOT/docker"
RUNNERS_MOUNT="$MOUNT_ROOT/runners"

# READ-ONLY: checks the operator's own dataset against what docker is about
# to be pointed at (mountpoint, sync). Never creates, sets, or destroys
# anything -- a mismatch is a hard stop naming the `zfs create`/`zfs set`
# the operator runs themselves, not a thing this script fixes for them.
require_dataset() {
	local name="$1" want_mountpoint="$2" want_sync="$3"
	local got
	if ! got="$(zfs list -H -o mountpoint,sync "$name" 2> /dev/null)"; then
		echo "bootstrap.sh: dataset '$name' does not exist -- create it yourself first:" >&2
		echo "  zfs create -o mountpoint=$want_mountpoint -o sync=$want_sync $name" >&2
		exit 1
	fi
	local got_mountpoint="${got%%$'\t'*}" got_sync="${got##*$'\t'}"
	if [ "$got_mountpoint" != "$want_mountpoint" ]; then
		echo "bootstrap.sh: '$name' has mountpoint=$got_mountpoint, expected $want_mountpoint -- fix it yourself:" >&2
		echo "  zfs set mountpoint=$want_mountpoint $name" >&2
		exit 1
	fi
	if [ "$got_sync" != "$want_sync" ]; then
		echo "bootstrap.sh: '$name' has sync=$got_sync, expected $want_sync -- fix it yourself:" >&2
		echo "  zfs set sync=$want_sync $name" >&2
		exit 1
	fi
	echo "bootstrap.sh: $name is at $want_mountpoint (sync=$want_sync), as expected"
}

require_dataset "$POOL/docker" "$DOCKER_MOUNT" standard
require_dataset "$POOL/runners" "$RUNNERS_MOUNT" disabled

if systemctl is-active --quiet docker; then
	CURRENT_ROOT="$(docker info --format '{{.DockerRootDir}}' 2> /dev/null || echo '')"
	if [ "$CURRENT_ROOT" != "$DOCKER_MOUNT" ] && [ -d /var/lib/docker ] && [ -n "$(ls -A /var/lib/docker 2> /dev/null)" ]; then
		echo "bootstrap.sh: stopping docker to migrate its store to $DOCKER_MOUNT"
		systemctl stop docker
		rsync -aHAX --info=progress2 /var/lib/docker/ "$DOCKER_MOUNT/"
	fi
fi

# Merge, never overwrite -- the host's daemon.json may already carry settings
# this directory does not know about (registry mirrors, log driver, etc).
DAEMON_JSON=/etc/docker/daemon.json
mkdir -p "$(dirname "$DAEMON_JSON")"
if [ -f "$DAEMON_JSON" ]; then
	jq -s '.[0] * .[1]' "$DAEMON_JSON" "$HERE/daemon.json" \
		| jq --arg root "$DOCKER_MOUNT" '.["data-root"] = $root' > "${DAEMON_JSON}.new"
else
	jq --arg root "$DOCKER_MOUNT" '.["data-root"] = $root' "$HERE/daemon.json" > "${DAEMON_JSON}.new"
fi
mv "${DAEMON_JSON}.new" "$DAEMON_JSON"
echo "bootstrap.sh: merged daemon.json (data-root=$DOCKER_MOUNT)"

install -Dm644 "$HERE/docker.service.d-pool.conf" /etc/systemd/system/docker.service.d/pool.conf
# The override's RequiresMountsFor names /mnt/pool literally -- rewrite it if
# this host uses a different mount root.
sed -i "s#/mnt/pool#$MOUNT_ROOT#g" /etc/systemd/system/docker.service.d/pool.conf

systemctl daemon-reload
systemctl restart docker

sleep 2
ACTUAL="$(docker info --format '{{.DockerRootDir}} {{.Driver}}')"
echo "bootstrap.sh: dockerd reports: $ACTUAL"
case "$ACTUAL" in
	"$DOCKER_MOUNT"*zfs) echo "bootstrap.sh: OK -- store is on the pool, driver is zfs" ;;
	*)
		echo "bootstrap.sh: REFUSING to declare success -- expected data-root under $DOCKER_MOUNT with driver zfs, got: $ACTUAL" >&2
		exit 1
		;;
esac

if ! docker info --format '{{json .SecurityOptions}}' | grep -q 'name=userns'; then
	echo "bootstrap.sh: FAILED -- daemon.json set userns-remap but dockerd did not apply it; check journalctl -u docker" >&2
	exit 1
fi
echo "bootstrap.sh: userns-remap is active"

cat <<EOF

Next: point webhook-runner at this host --
  WEBHOOK_RUNNER_SCRATCH_DIR=$RUNNERS_MOUNT
  WEBHOOK_RUNNER_EXPECT_DATA_ROOT=$MOUNT_ROOT

For the gha-runner-dind fleet specifically: dind: true no longer implies
--privileged (see docs/internals/runner-isolation.md, "What that costs"), so
its nested dockerd needs sysbox-runc on this host:
  https://github.com/nestybox/sysbox/blob/master/docs/user-guide/install-package.md
That is a separate install this script does not perform.
EOF
