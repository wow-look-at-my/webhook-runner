# Runtime image for webhook-runner.
#
# The binary is built upstream by go-toolchain (CGO_ENABLED=0, static) and
# downloaded into build/ by the publish-ghcr workflow. This Dockerfile only
# packages that prebuilt artifact, mirroring the buildhost pattern. The
# runtime needs docker-cli, git, and ssh because the server shells out to
# `docker run` for each hook and clones/pulls the hooks repo over SSH.

FROM alpine:3.20
RUN apk add --no-cache docker-cli git openssh-client ca-certificates tzdata && \
    addgroup -S webhook && adduser -S -G webhook webhook

ARG VERSION=dev
LABEL org.opencontainers.image.source="https://github.com/wow-look-at-my/webhook-runner"
LABEL org.opencontainers.image.version="${VERSION}"
LABEL org.opencontainers.image.licenses="MIT"
LABEL org.opencontainers.image.description="Executes incoming webhooks inside disposable Docker containers"

COPY --chmod=755 build/webhook-runner_linux_amd64 /usr/local/bin/webhook-runner

ENV WEBHOOK_RUNNER_ADDR=":9000" \
    WEBHOOK_RUNNER_ADMIN_ADDR=":9001" \
    WEBHOOK_RUNNER_LOG_FORMAT="text"

# Runs as root by default so it can talk to the bind-mounted Docker socket.
ENTRYPOINT ["/usr/local/bin/webhook-runner"]
