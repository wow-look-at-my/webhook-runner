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
bind does, and only while the container runs as root does it matter. Exactly two
docker flags remove the bind:

- `--privileged`
- `--security-opt systempaths=unconfined`

Both have been in this runner. Neither is now, and
`internal/runner/hostprimitives_test.go` fails the build if either comes back —
on the live-run path and the `webhook-runner test` path alike, with every
privilege-adjacent manifest field switched on. The check is mechanical because
the argv is assembled in three places and a reviewer reading one cannot see the
other two.

## What that costs

A nested container daemon needs those capabilities. `dind: true` therefore no
longer grants them: it means one thing, an anonymous volume at
`/var/lib/docker`, because an inner daemon cannot stack its overlay driver on
the outer container's overlay rootfs. An image that wants a nested daemon runs a
**rootless** one (`dockerd-rootless.sh`, needing a user namespace,
fuse-overlayfs or vfs storage, and no iptables). A root `dockerd` does not come
up and the run fails saying so — an unconverted image fails every run, which is
the honest signal that the conversion has not happened.

## Defence in depth: userns-remap

With `{"userns-remap": "default"}` in `daemon.json`, container root maps to an
unprivileged host uid. The `core_pattern` write is then refused on the DAC
check, because the process is not uid 0 in the initial user namespace — the same
for `/proc/sysrq-trigger`, `/sys`, and every other global. It is the layer that
still holds when one of the read-only binds is wrong.

It is reported, not enforced: `CheckUsernsRemap` logs what the daemon says and
raises a needs-attention entry when the answer is no. Refusing to serve over it
would take down every hook — pr-minder, required-builds, the lot — for a
hardening step, which is a larger outage than the risk it removes.

Compatibility, checked against this fleet: no manifest uses `networks`, and the
runner passes no host-namespace flag, so nothing here conflicts with remap. Note
the store gains a subuid-owned subdirectory (`<data-root>/<uid>.<gid>/`);
`WEBHOOK_RUNNER_EXPECT_DATA_ROOT` compares by prefix, so it still matches.

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
