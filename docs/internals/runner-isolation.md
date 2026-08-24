# Keeping a runner container off the host

The gha-runner fleets execute other people's CI. The requirement is narrow and
concrete: **no one-line command inside a container may disturb the host.** Not
"a kernel exploit is impossible" — a shared kernel cannot promise that — but the
cheap kind:

```
echo b > /proc/sysrq-trigger                  reboots the host
echo '|/x' > /proc/sys/kernel/core_pattern    runs /x as real root on the host
```

Both files are global. Neither is namespaced. `core_pattern` is the worse of
the two: the **host** kernel runs the named helper as real root on any core
dump, and a `scratch` mount already gives the container a writable host path to
point it at.

## What actually stops them

Docker binds `/proc/bus`, `/proc/fs`, `/proc/irq`, `/proc/sys` and
`/proc/sysrq-trigger` **read-only**, and masks a handful of other paths under
`/proc`. That bind is the entire protection, and it is thinner than it looks.

Measured on kernel 6.18.44, writing `core_pattern` as uid 0:

| | result |
|---|---|
| root, full capabilities | write permitted |
| root, `CAP_SYS_ADMIN` dropped | **write permitted** |
| unprivileged uid | write refused |

So docker's dropped capability set never guarded that file. Only the read-only
bind does — **and only while the container's root is the host's root does that
matter.** That second clause is the whole design below.

Two docker flags remove the bind, and they are treated differently:

- `--privileged` is **banned outright**. It grants every capability, full device
  access and an unmasked `/proc` in one flag, and nothing here needs that.
- `systempaths=unconfined` is **required** for an in-container sandbox, so it
  ships behind an interlock rather than a ban: only on a daemon that remaps
  container root.

`internal/runner/hostprimitives_test.go` pins both, on the live-run path and the
`webhook-runner test` path alike, with every privilege-adjacent manifest field
switched on. It asserts the interlock, not the absence of a flag: a userns run
is refused without remap and granted with it. The check is mechanical because
the argv is assembled in three places and a reviewer reading one cannot see the
other two.

## bwrap, and why it needs remap

A hook that runs its own sandbox — bubblewrap, and so `dats` on its default
backend — cannot work while those binds are in place: the kernel refuses a fresh
procfs mount inside a user namespace whenever the visible `/proc` is obstructed.
bwrap dies on `Can't mount proc on /newroot/proc: Operation not permitted` with
every syscall it needs already allowed.

Clearing them is the only fix docker offers, and it is safe exactly when the
container's root is not the host's root. Measured, with `/proc` fully unmasked:

| | mapped root (userns-remap) | container root == host root |
|---|---|---|
| `core_pattern` | refused | **write permitted** |
| `/proc/sysrq-trigger` | refused | **write permitted** |
| `/proc/kcore` | refused | readable |
| `bwrap --proc` | **OK** | OK |

The rule underneath: capabilities held in a user namespace apply only to files
owned by a uid **mapped into** that namespace. Under remap, a host-root-owned
file sits outside the container's map, so `CAP_DAC_OVERRIDE` in the namespace
does not reach it. Without remap the map includes uid 0 and it does.

So `seccomp.userns` emits `systempaths=unconfined` **only** on a remapped
daemon, and a hook that declares it on a daemon without remap **fails its run**,
naming `userns-remap`. Half-configuring it is the outcome worth refusing: with
the masks the sandbox cannot be built, and without remap the job owns the
machine. The interlock is pinned by `TestUsernsRunIsRefusedWithoutRemap` and
`TestUsernsRunGetsAnUnmaskedProcUnderRemap`, and `webhook-runner test` applies
it too, so an entity this daemon cannot serve fails in CI rather than on a
runner.

**userns-remap is therefore required, not advisory, for any host running the
runner fleets.** `deploy/pool-storage/` sets it. `CheckUsernsRemap` still only
reports at startup — refusing to serve over it would take down pr-minder and
required-builds, which never ask for a sandbox.

## What that costs

A nested container daemon needs the capabilities `--privileged` grants. `dind:
true` therefore no longer grants them: it means one thing, an anonymous volume
at `/var/lib/docker`, because an inner daemon cannot stack its overlay driver on
the outer container's overlay rootfs. An image that wants a nested daemon runs a
**rootless** one (`dockerd-rootless.sh`, needing a user namespace,
fuse-overlayfs or vfs storage, and no iptables). A root `dockerd` does not come
up and the run fails saying so — an unconverted image fails every run, which is
the honest signal that the conversion has not happened.

[sysbox](https://github.com/nestybox/sysbox) is the cheapest way back: a drop-in
OCI runtime that gives per-container user namespaces and unprivileged nested
docker, with no image conversion.

## Storage

Container writes land wherever the daemon is rooted, so the pool is a daemon
property, not a per-container one. `deploy/pool-storage/` covers the two
datasets: the store on `<pool>/docker` at `sync=standard`, and
`WEBHOOK_RUNNER_SCRATCH_DIR` on `<pool>/runners` at `sync=disabled`. Losable
data gets the losable dataset; images and volumes do not.
`docs/internals/scratch-dirs.md` has the depth.

## The boundary this does not provide

Everything above raises the floor on a shared kernel. A kernel LPE still reaches
the host, and no arrangement of docker flags changes that for code you did not
write.

The boundary that does is a **virtual machine per job**: its own kernel, its own
rootfs, and a guest `reboot` that kills the microVM rather than the host.
Firecracker is the obvious fit — it is what AWS Lambda and GitHub's own hosted
runners use, it boots in ~125 ms, and a job's disk is a device backed by a file
or zvol on `<pool>/runners`, which satisfies the storage rule by construction
rather than by configuration.

The shape, if it gets built: the `gha-runner` hook stops running the actions
runner in its own container and instead boots a microVM per queued job, passing
the JIT registration token in through the guest's config. What it needs from the
host is `/dev/kvm`, a kernel image, and a rootfs template. What it costs is a
guest image to build and maintain, and a new failure surface between the hook
and the VM. Nothing in this repo does it yet, and nothing here pretends to.
