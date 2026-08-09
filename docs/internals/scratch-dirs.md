# Keeping hook writes off docker's data-root

A hook run writes to three places, and by default all three are the same
disk:

1. the container's **writable layer** (everything the process writes that is
   not a mount), which lives under docker's `data-root`;
2. **anonymous volumes**, e.g. the `dind` hook's `/var/lib/docker`, which
   live under `data-root/volumes`;
3. explicit **mounts**, which live wherever they point.

For most hooks that is fine. For the CI-runner fleets it is not: a
`gha-runner` job checks out a repo, installs dependencies and builds, and a
`gha-runner-dind` job additionally pulls whole images into a *fresh* store
every run. That traffic is continuous, large, and write-heavy, and on a host
whose `data-root` sits on a disk that should not absorb it, it is the
dominant source of wear.

Three manifest fields move it, and one of them is what makes the other two
trustworthy.

## The fields

```jsonc
{
  "scratch": ["/home/runner/_work", "/var/lib/docker"],
  "tmpfs": ["/tmp", "/home/runner/.cache", "/home/runner/.npm"],
  "read_only_rootfs": true
}
```

- **`scratch`** — absolute container paths bind-mounted from a directory
  private to this run, created under `WEBHOOK_RUNNER_SCRATCH_DIR` before the
  container starts and removed when it ends. For the writes too big for
  memory.
- **`tmpfs`** — absolute container paths backed by RAM. For everything else.
  Contents vanish with the container.
- **`read_only_rootfs`** — `--read-only`, so the *only* writable locations
  are the mounts above.

## Why read_only_rootfs is the load-bearing one

Without it, `scratch` and `tmpfs` describe the paths somebody remembered to
list. Any write to a path nobody listed still lands in the container's
writable layer, on the very disk the declarations exist to spare, and
**nothing reports it** — the run passes, the job passes, and the only symptom
is disk wear nobody can attribute.

With it, an unlisted write fails loudly at the moment it happens. That turns
"we think we moved the writes" into "an unmoved write cannot run".

It is opt-in because it can only be established per image: a program that
writes somewhere unlisted breaks under it. That is what the hook's own
`tests` are for — `webhook-runner test` applies these same flags, so a hook
that has not accounted for its writes fails in CI, before the fleet runs it.
On the test path `scratch` paths are backed by tmpfs rather than the scratch
filesystem: a CI runner has no such filesystem, and what the test needs to
establish is that every writable path IS a mount, not which disk backs it.

## Operator setup

```
WEBHOOK_RUNNER_SCRATCH_DIR=/mnt/pool/whr-scratch
```

(or `--scratch-dir`). Unset, a run whose hook declares `scratch` **fails**.
That is deliberate: running it anyway would put exactly the writes the
declaration exists to divert back on the wrong disk, invisibly, which is the
outcome the field exists to prevent. A failed run says so; a silently
misplaced write does not.

**If webhook-runner itself runs in a container**, the path must be the same
absolute path on the host and inside that container. The runner passes mount
sources to the host's docker daemon, so the daemon resolves them in the
host's namespace — the same hazard `TmpDirHazardMessage` covers for the
per-run payload mounts. Bind the filesystem at the identical path in both
places.

Layout on disk, during a run:

```
/mnt/pool/whr-scratch/
  run-<run id>/
    0-home_runner__work/     -> /home/runner/_work
    1-var_lib_docker/        -> /var/lib/docker
```

The index prefix is what keeps leaf names unique: two distinct container
paths can sanitize to the same string (`/a/b` and `/a_b`).

## Lifecycle

- Created after the run holds its concurrency slots, so a long queue does not
  park empty directories on the filesystem.
- Removed when the run ends, however it ends.
- Swept at serve startup (`SweepOrphanScratch`), for the subtrees a process
  that died mid-run left behind. Same safety argument as the orphan-container
  sweep: the run store's bbolt flock proves no other serve process is live,
  and this one has started no runs yet, so every `run-*` directory present is
  by definition an orphan. Entries not named `run-*` are left alone — the
  scratch root may be a dataset the operator keeps other things on.

Without that sweep this is the one leak that grows without bound: an orphaned
container holds an IP until the next boot, an orphaned scratch subtree holds
a whole job tree forever.

## Interaction with dind

A `dind` hook normally gets `--privileged` plus an anonymous volume at
`/var/lib/docker`. When the hook lists `/var/lib/docker` in `scratch`, the
anonymous volume is **not** added — two mounts on one destination is a docker
error, so getting this wrong fails every dind run outright. `--privileged` is
unaffected.

## What this does NOT do

These fields relocate mounts. They do **not** relocate the container's
writable layer itself, because docker has no per-container setting for it:
the writable layer's location is a property of the daemon (`data-root`), not
of `docker run`. `read_only_rootfs` closes the gap from the other side — by
making the writable layer unusable rather than by moving it.

To relocate the writable layer for real, there are exactly two options, both
host-side:

**Move the daemon's `data-root`.** Everything moves — image layers, writable
layers, volumes — for every container on the host, not just hooks.

```jsonc
// /etc/docker/daemon.json
{ "data-root": "/mnt/pool/docker" }
```

```
systemctl stop docker
rsync -aHAX --info=progress2 /var/lib/docker/ /mnt/pool/docker/
systemctl start docker
docker info --format '{{.DockerRootDir}}'   # confirm before deleting the old tree
```

ZFS caveat, and it is not optional: **`overlay2` is unsupported on a ZFS
dataset.** Either point `data-root` at a **zvol formatted ext4 or xfs** (so
`overlay2` keeps working, which is the conservative choice), or use docker's
native **`zfs` storage driver**, which requires `data-root` to be its own
dataset and makes each layer a dataset of its own. Do not put `data-root` on
a plain ZFS dataset and leave `overlay2` configured.

**Or run a second daemon** rooted on the pool and launch only the runner
containers against it. Scoped to the hooks you choose, at the cost of a
second daemon to operate, and it needs a per-hook docker-host field that does
not exist today.
