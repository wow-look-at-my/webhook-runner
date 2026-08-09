# A second docker daemon whose store is on the pool

Everything webhook-runner creates is disposable: a hook run is one container
that exits, and its image rebuilds from the hook directory on demand. So the
whole store — image layers, container writable layers, volumes — can live on a
filesystem that is allowed to lose data, and putting it there takes ALL of a
hook's writes off the system disk in one move, including the ones no mount can
reach (`apt-get install`, anything under `/usr/local`).

This is a SECOND daemon, deliberately. The host's main daemon keeps its store
where it is.

## Why not just move the main daemon's data-root

Because `data-root` is docker's entire store for every container on the host,
and on a pool with `sync=disabled` a power loss can leave it torn. Image layers
and volumes belonging to things that are NOT reconstructible have no business
on a filesystem tuned for losable data.

A second daemon inverts that: the only thing on the pool is a store whose
entire contents rebuild from Dockerfiles. Losing it costs one rebuild.

## Install

`daemon.json` here says `/mnt/pool/docker`. Substitute your own path and pool
name throughout — and make the dataset's mountpoint equal `data-root`, since
the two are set independently and nothing warns when they disagree:

```
zfs create -o mountpoint=/mnt/pool/docker <pool>/docker   # its own dataset — the zfs driver requires it
cp daemon.json           /etc/docker/pool-daemon.json
cp docker-pool.service   /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now docker-pool
docker --host unix:///var/run/docker-pool.sock info --format '{{.DockerRootDir}} {{.Driver}}'
```

That last line must print your pool path and `zfs`. If it prints anything else,
stop: the daemon built its store somewhere other than the dataset.

Then point webhook-runner at it and tell it what to expect:

```
DOCKER_HOST=unix:///var/run/docker-pool.sock
WEBHOOK_RUNNER_EXPECT_DATA_ROOT=/mnt/pool
```

`DOCKER_HOST` is inherited by every docker command the runner shells out to —
build, run, inspect, kill, the startup orphan sweep — so one variable moves all
of it. `WEBHOOK_RUNNER_EXPECT_DATA_ROOT` is what makes being wrong loud: at
startup the runner asks the daemon where its store actually is, logs it, and
raises a needs-attention entry if it is not under that path. A DOCKER_HOST typo
or a unit that failed to start otherwise leaves the runner quietly talking to
the default daemon, writing to the disk this whole arrangement exists to spare
— and nothing else would ever say so.

If webhook-runner runs in a container, bind-mount the socket in and keep the
same `DOCKER_HOST` path.

## The settings that are not arbitrary

- **`storage-driver: zfs`** — `overlay2` is UNSUPPORTED on a ZFS dataset. The
  native driver needs `data-root` to be its own dataset (hence the
  `zfs create` above) and makes each layer a dataset, which is why layer reuse
  stays cheap. The alternative is a zvol formatted ext4/xfs with `overlay2`;
  pick one, do not put `overlay2` on a plain dataset.
- **`bridge` / `bip`** — a second daemon must not share `docker0` or the
  default `172.17.0.0/16`, or the two daemons fight over the same bridge and
  addresses. Change `bip` if it collides with something else on the host.
- **`hosts` / `pidfile` / `exec-root`** — all distinct from the main daemon's,
  for the same reason.
- **`RequiresMountsFor=/mnt/pool`** in the unit — without it a boot race starts
  dockerd against an empty mountpoint and it creates its store on the root
  filesystem underneath, silently, which is the failure this daemon exists to
  prevent.
- **`live-restore: false`** — containers here are disposable and the runner
  reaps orphans at startup; keeping them alive across a daemon restart would
  strand containers no server owns.

## If the store is damaged

A pool with `sync=disabled` can lose recent writes on power loss, and a torn
docker store does not always fail cleanly — the daemon may refuse to start or
serve a half-written layer. That is an accepted cost here rather than a
disaster, and the recovery is to throw it away:

```
systemctl stop docker-pool
zfs destroy -r <pool>/docker && zfs create -o mountpoint=/mnt/pool/docker <pool>/docker
systemctl start docker-pool
```

Every hook image rebuilds from its directory on the next run. Nothing in this
store is the only copy of anything, which is the entire reason it is allowed to
live on a filesystem tuned for losable data — and the reason the host's main
daemon must NOT be moved here.

## What still is not on the pool

webhook-runner's own container and image, if it runs as one — those live on
whatever daemon started it, which is the main one. That is correct: the server
is not disposable.
