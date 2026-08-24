# Putting a runner host's writes on the pool

Two datasets, one docker daemon. The split is what the sync properties are for:

| dataset | `sync` | holds | why |
|---|---|---|---|
| `<pool>/docker` | `standard` | docker's whole store — image layers, container writable layers, volumes | this is where an unlisted write lands, including a CI job's `apt-get install` and anything under `/usr/local` |
| `<pool>/runners` | `disabled` | `WEBHOOK_RUNNER_SCRATCH_DIR`: `_work`, dind's nested `/var/lib/docker` | bulk, high-churn, rebuilt every run — the data that is genuinely allowed to be lost |

Only the runner scratch is `sync=disabled`. `sync` is a per-DATASET property, so
this leaves the rest of the pool exactly as it is.

## Why the docker store has to move at all

A container's writable layer IS an overlayfs upperdir, and its location is a
DAEMON property (`data-root`) — there is no per-container knob. `scratch` and
`tmpfs` in a manifest relocate named paths, so everything they do not name still
lands wherever the daemon is rooted. Moving `data-root` is the only thing that
covers the writes nobody enumerated.

## Install

`bootstrap.sh` never runs `zfs create`, `zfs set`, `zfs destroy`, or any
`zpool` command — the operator owns the pool and its datasets, configured
however they need them, and this script only ever reads them (`zfs list`) to
check they match what docker is about to be pointed at. Create the two
datasets yourself first:

```
zfs create -o mountpoint=/mnt/pool/docker -o sync=standard <pool>/docker
zfs create -o mountpoint=/mnt/pool/runners -o sync=disabled  <pool>/runners
```

(`daemon.json` here says `/mnt/pool/docker`. Substitute your own path and pool
name throughout — and make the dataset's mountpoint equal `data-root`, since the
two are set independently and nothing warns when they disagree.)

Then `bootstrap.sh <pool> [mount-root]` runs the rest as one idempotent
script — verify both datasets exist with the right mountpoint/`sync`, migrate
the existing docker store, merge `daemon.json`, install the systemd override,
restart dockerd, and verify `data-root`/driver/`userns-remap` all actually
took. Run it as root on the runner host; it refuses to declare success on a
mismatch, and refuses to touch docker at all if either dataset is missing or
misconfigured — naming the exact `zfs create`/`zfs set` line to run, which it
does not run for you. It does not install sysbox either (see the `dind` note
below).

The manual steps it automates past dataset verification, for reference or if
you want to run them by hand instead:

```
systemctl stop docker                       # a live daemon will not follow its store
rsync -aHAX /var/lib/docker/ /mnt/pool/docker/   # or skip it and re-pull everything
```

Merge `daemon.json` into `/etc/docker/daemon.json` (it is not a whole file —
keep whatever the host already had), then install the mount ordering:

```
install -Dm644 docker.service.d-pool.conf /etc/systemd/system/docker.service.d/pool.conf
systemctl daemon-reload && systemctl start docker
docker info --format '{{.DockerRootDir}} {{.Driver}}'
```

That last line must print your pool path and `zfs`. If it prints anything else,
stop: the daemon built its store somewhere other than the dataset.

Then tell webhook-runner what to expect:

```
WEBHOOK_RUNNER_SCRATCH_DIR=/mnt/pool/runners
WEBHOOK_RUNNER_EXPECT_DATA_ROOT=/mnt/pool
```

`WEBHOOK_RUNNER_EXPECT_DATA_ROOT` is what makes being wrong loud: at startup the
runner asks the daemon where its store actually is, logs it, and raises a
needs-attention entry if it is not under that path. A `daemon.json` that did not
parse, or a mount that was not there when dockerd started, otherwise leaves the
runner writing to the system disk this whole arrangement exists to spare — and
nothing else would ever say so.

## The settings that are not arbitrary

- **`storage-driver: zfs`** — it is the driver that does not depend on a kernel
  feature check. The native driver makes each layer a dataset and clones it, so
  no overlay mount happens and no upperdir requirement applies. It needs
  `data-root` to be its own dataset, which is why `<pool>/docker` exists.

  `overlay2` is the other candidate and it is CONDITIONAL. Docker's `overlay2`
  is overlayfs, and overlayfs refuses an upperdir on a filesystem that cannot do
  `tmpfile` and `RENAME_WHITEOUT`. OpenZFS added `RENAME_WHITEOUT` in 2.2, so
  whether the mount is accepted depends on the ZFS version on the host. This
  script does not probe for it and never will — that means creating and
  destroying a throwaway dataset, and dataset lifecycle here is the
  operator's call, not something run on their pool by anything in this repo.
  If you want to evaluate `overlay2` yourself: create a scratch dataset with
  your own tooling, mount an overlay on it (`lowerdir`/`upperdir`/`workdir`
  under it), and check whether the mount is accepted or refused with
  `wrong fs type, bad option` (`dmesg` names the missing feature on refusal).
  A zvol formatted ext4 or xfs carries `overlay2` on any ZFS version, at the cost
  of a fixed-size second filesystem.

  The same probe answers a hand-rolled overlay — bubblewrap's `--overlay`, or a
  FUSE filesystem standing in as the upper. They are the same kernel mount with
  the same feature check, so they pass or fail exactly where this does, and none
  of them relocates the writes `data-root` already covers.

- **`userns-remap: default`** -- REQUIRED, not hardening. Container root maps
  to an unprivileged host uid, which is what makes it safe to clear docker's
  masked and read-only `/proc` paths, which is in turn what bubblewrap needs to
  mount a procfs in its own namespace. A hook declaring `seccomp.userns` FAILS
  its run on a daemon without it, naming this setting. Measured: a mapped root
  is refused `core_pattern`, `/proc/sysrq-trigger` and `/proc/kcore` while bwrap
  still works; an unmapped root writes the first two. Note the store gains a
  subuid-owned subdirectory (`/mnt/pool/docker/<uid>.<gid>/`);
  `WEBHOOK_RUNNER_EXPECT_DATA_ROOT` matches by prefix, so it still resolves.
  Nothing in this fleet conflicts with it: no manifest uses `networks` and the
  runner passes no host-namespace flag. Depth:
  `docs/internals/runner-isolation.md`.
- **`RequiresMountsFor=/mnt/pool`** — without it a boot race starts dockerd
  against an empty mountpoint and it creates its store on the root filesystem
  underneath, silently, which is the failure this whole arrangement exists to
  prevent.

## dind after this: install sysbox

`gha-runner-dind`'s nested dockerd needs `--privileged` to prep cgroup-v2
nesting, and `bootstrap.sh` does not grant that -- host isolation is the
point. Install [sysbox-runc](https://github.com/nestybox/sysbox) on this
same host to get it back without `--privileged`: it gives each container its
own user namespace and an unprivileged nested docker, needs no image
changes, and is compatible with `userns-remap` (not required alongside it as
of sysbox 0.5+). See its
[install guide](https://github.com/nestybox/sysbox/blob/master/docs/user-guide/install-package.md).
Until this is done, `gha-runner-dind`'s own declared tests
(`dockerd-smoke.test.ts`, `dats-smoke.test.ts`) fail loudly rather than
silently -- that failure is this gap, not a regression.

## What is still not on the pool

webhook-runner's own container and image, if it runs as one, live on whatever
daemon started it. Once `data-root` moves, that is this same daemon, so they
move too — the server is not disposable, but it is also not large, and it is
`sync=standard` here like everything outside `<pool>/runners`.
