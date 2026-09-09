# Runtime image for webhook-runner.
#
# The binary is built upstream by go-toolchain (CGO_ENABLED=0, static) and
# downloaded into build/ by the publish-ghcr workflow. This Dockerfile only
# packages that prebuilt artifact, mirroring the buildhost pattern. The
# runtime needs docker-cli, git, and ssh because the server shells out to
# `docker run` for each hook and clones/pulls the hooks repo over SSH.
# docker-cli-buildx is REQUIRED, not optional: THIS CLI drives every hook image
# build, and without the plugin it silently falls back to the legacy builder,
# which cannot parse `# syntax=` frontends or flags like `ADD --unpack` however
# capable the host daemon is. It also

# needs sops to decrypt per-hook `secrets.sops.env` files: the server execs the
# `sops` binary host-side (hooks.SecretsLoader), then injects the decrypted
# values into the hook container as plain env vars — the hook container itself
# never sees sops. sops decrypts age natively, so `age` isn't strictly required
# for that path; it's included for key generation/inspection during ops. The age
# *identity* (the private key) is supplied at runtime via SOPS_AGE_KEY_FILE and
# is never baked into the image.

FROM alpine:3.20
RUN apk add --no-cache docker-cli docker-cli-buildx git openssh-client ca-certificates tzdata sops age && \
    addgroup -S webhook && adduser -S -G webhook webhook

ARG VERSION=dev
LABEL org.opencontainers.image.source="https://github.com/wow-look-at-my/webhook-runner"
LABEL org.opencontainers.image.version="${VERSION}"
LABEL org.opencontainers.image.licenses="MIT"
LABEL org.opencontainers.image.description="Executes incoming webhooks inside disposable Docker containers"

# HACK, pending the real fix upstream. A bare ENTRYPOINT on the APE
# go-toolchain builds exits with "exec format error" on this host. The APE
# therefore sits under /usr/local/lib and a shebang launcher takes its place at
# the entrypoint path, so a container recreated from an older container's
# config still names /usr/local/bin/webhook-runner and still starts.
COPY --chmod=755 build/webhook-runner /usr/local/lib/webhook-runner/webhook-runner
COPY --chmod=755 scripts/image-launcher.sh /usr/local/bin/webhook-runner

# The KV state store is served on an internal Unix socket (under TMPDIR), not a
# port: state hooks reach it at a plain http://localhost:9002 via a proxy shim
# the runner injects, so there is nothing to publish. TMPDIR must be host-shared
# (same as payload files); set WEBHOOK_RUNNER_DATA_DIR to a persistent volume so
# KV state and the token secret survive restarts. See the README.
ENV WEBHOOK_RUNNER_ADDR=":9000" \
    WEBHOOK_RUNNER_ADMIN_ADDR=":9001" \
    WEBHOOK_RUNNER_LOG_FORMAT="text"

# Probe the hook port's /health endpoint (busybox wget ships with alpine).
# Shell form so the port tracks WEBHOOK_RUNNER_ADDR at runtime. Without a
# HEALTHCHECK, orchestrators like docker-updater have no health signal to
# gate deploys on.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -q -O /dev/null "http://127.0.0.1:${WEBHOOK_RUNNER_ADDR##*:}/health" || exit 1

# Metadata only -- this publishes nothing on the host. Both ports are declared
# because both exist; the /.well-known/docker-updater/ endpoints live on the
# admin port beside /restart-ready, so a deployment has to name it with
# docker-updater.well-known.port: "9001" -- discovery only picks a port by
# itself when an image declares exactly one.
EXPOSE 9000 9001

# Runs as root by default so it can talk to the bind-mounted Docker socket.
ENTRYPOINT ["/usr/local/bin/webhook-runner"]
