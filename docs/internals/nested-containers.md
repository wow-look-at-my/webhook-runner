# What an unprivileged hook container can host

`dind: true` gives a container storage and nothing else (`internal/runner/dind.go`).
`--privileged` is refused by operator ruling. This page records what that leaves
possible, measured on docker 29.3.1 / kernel 6.18, so the next person does not
have to rediscover it from a stack of confusing EPERMs.

## The short version

| | works unprivileged? |
|---|---|
| bubblewrap sandbox (dats' default backend) | yes, with `seccomp.userns` |
| rootless `dockerd` coming up, `docker version`, `docker load` | yes |
| the nested daemon RUNNING a container | **no** |

The daemon starting and the daemon working are different questions, and the
gap between them is one kernel check.

## The wall: a fresh procfs

`runc` mounts a fresh `proc` on every container's rootfs. The kernel refuses
that mount when the caller's own `/proc` carries locked submounts it would
bypass — `mount_too_revealing()` in `fs/namespace.c`. Docker's defaults put ten
of them on every container:

    /proc/bus  /proc/fs  /proc/irq  /proc/sys  /proc/sysrq-trigger
    /proc/acpi  /proc/interrupts  /proc/keys  /proc/timer_list  ...

So inside a hook container, in a user namespace where the process holds
`CAP_SYS_ADMIN` over that namespace:

    $ unshare -U -r -m -p -f --mount-proc true
    unshare: mount /proc failed: Operation not permitted

A new PID namespace does not help, and neither does nesting deeper: the locked
mounts belong to the initial user namespace, and a mount namespace created
below it cannot unmount or shadow them. `runc` hits the identical refusal:

    error mounting "proc" to rootfs at "/proc": operation not permitted

The one docker flag that clears those mounts is `systempaths=unconfined`, which
also makes `/proc/sysrq-trigger` writable — one write reboots the host. It is
banned here, in `internal/runner/bannedflags_test.go`.

## How far rootless docker does get

Everything except that last step, which is worth knowing because the failures
on the way there look fatal and are not:

- **Run rootlesskit as the container's own root.** Its `newuidmap` helper fails
  as an ordinary user here, so the supported non-root shape does not start;
  container root writes the multi-range `uid_map` itself and rootlesskit
  proceeds (with an "unsupported" warning).
- **`--slirp4netns-sandbox=false --slirp4netns-seccomp=false`.** slirp4netns's
  own sandbox needs the same refused mounts.
- **`--device /dev/net/tun`, chmod 0666.** The node arrives root-only and the
  daemon does not run as root.
- **Daemon storage on a volume.** `dind`'s volume, for the overlay-on-overlay
  reason above; without it the first `docker run` dies on `invalid argument`.
- **`keyctl`/`add_key`/`request_key`.** `runc` joins a session keyring, and
  moby's default seccomp profile gates those on `CAP_SYS_ADMIN`.

With all of that the daemon reports `driver=overlayfs`, `rootless`, and comes
up in about a second. Then the first container hits the procfs wall.

## What would actually buy the capability back

Not a flag — a different isolation boundary. A runtime that virtualizes `/proc`
for the container (sysbox), or a VM per runner (Firecracker, Kata). Both are
host-side changes, not hook.json fields, and neither is deployed today.
