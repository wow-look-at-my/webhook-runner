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

## What no mount can reach

Before reaching for either field, know the ceiling: a mount can only redirect a
path you are willing to REPLACE. System paths fail that test. `/usr/local`
holds the image's installed toolchain, `/usr` and `/var/lib/apt` are where
`apt-get install` lands, and mounting over any of them takes away the very
thing the container was built to provide. So a job that installs a package
writes to the container's layer no matter what these fields say.

`read_only_rootfs` does not rescue that case, and it is important not to read
it as if it did: it FORBIDS the write, it does not relocate it. On a
general-purpose CI image that means the package install fails rather than
landing somewhere cheaper. It is the right tool for an image whose writes you
have fully enumerated, and the wrong tool for one that runs arbitrary jobs.

Relocating the writable layer itself is the only thing that covers system
writes, and that is the host-side `data-root` move at the bottom of this file.

## Why read_only_rootfs is the load-bearing one when it fits

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

**The writable layer already IS an overlayfs upper dir.** Under `overlay2`
every container writes into `<data-root>/overlay2/<id>/diff`, with the image
layers as lowerdirs. So "overlay the filesystem onto tmpfs" is not something to
build on top of this — it is the arrangement docker already runs, and the only
question is where its upperdir lives. That is also why the fields above are a
partial substitute: they hand-mount individual paths to work around one overlay
that already covers every path.

Two consequences worth stating plainly, because both look like ways out and
are not:

- **Doing the overlay inside the container** (remounting `/usr` and friends
  onto a tmpfs upper from the entrypoint) needs `CAP_SYS_ADMIN`. A privileged
  hook could; an unprivileged one cannot, and unprivileged overlay needs a user
  namespace a stock container is refused. It would also reimplement, per hook
  and per path, what the daemon does once for everything.
- **Running work inside a nested daemon** whose store is on the pool moves the
  NESTED containers' layers, not the outer container's. A CI job runs in the
  outer container unless it explicitly asks for a container step, so its own
  writes are untouched by that.

Relocating it is a daemon-level change, and WHICH daemon matters:

**Run a second daemon whose store is on the transient filesystem, and point
`DOCKER_HOST` at it.** This is the supported arrangement —
`deploy/pool-daemon/` ships the config, the unit and the steps. Every docker
command the runner shells out to inherits `DOCKER_HOST`, so build, run,
inspect, kill and the orphan sweep all follow it; set
`WEBHOOK_RUNNER_EXPECT_DATA_ROOT` alongside it so a typo or a dead unit raises
a needs-attention entry instead of silently falling back to the default daemon
(`CheckDataRoot`, dataroot.go).

It fits because everything this runner creates is disposable: a hook run is one
container that exits, and its image rebuilds from the hook directory. The only
thing on that filesystem is a store whose entire contents can be regenerated,
so losing it costs a rebuild — which is what makes it safe to put on a pool
tuned for losable data.

**Do NOT move the MAIN daemon's `data-root` there instead.** That is docker's
whole store for every container on the host, including images and volumes
belonging to things that are not reconstructible, and a filesystem configured
for transient data (e.g. ZFS with `sync=disabled`) can lose writes on power
loss. The second daemon exists precisely so the losable store and the durable
one are different stores.

**A tmpfs-backed store** is the strongest form of "writes never touch a disk"
and takes the IMAGE layers with it, since docker cannot split them from the
upper dirs. For multi-GB runner images that trades a disk problem for a memory
one and re-pulls everything after a reboot.

ZFS caveat, and it is not optional: **`overlay2` is unsupported on a ZFS
dataset.** Use docker's native **`zfs` storage driver** with `data-root` on its
own dataset, or a **zvol formatted ext4/xfs** with `overlay2`. Do not put
`overlay2` on a plain dataset.
