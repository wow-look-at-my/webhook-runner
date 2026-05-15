# syntax=docker/dockerfile:1.7
#
# Multi-stage build for webhook-runner.
#
# - Build stage uses golang:1.24-alpine to produce a statically-linked
#   binary (CGO_ENABLED=0).
# - Runtime stage is alpine + docker-cli; the running container needs
#   the docker socket bind-mounted from the host so it can shell out
#   to `docker run` for each hook.

FROM golang:1.24-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS="-trimpath"

# Cache the module download separately from the source for fast
# incremental rebuilds.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN go build \
    -ldflags="-s -w -X github.com/wow-look-at-my/webhook-runner/internal/cli.version=${VERSION}" \
    -o /out/webhook-runner \
    ./cmd/webhook-runner

FROM alpine:3.20
RUN apk add --no-cache docker-cli git ca-certificates tzdata && \
    addgroup -S webhook && adduser -S -G webhook webhook
COPY --from=build /out/webhook-runner /usr/local/bin/webhook-runner

ENV WEBHOOK_RUNNER_ADDR=":9000" \
    WEBHOOK_RUNNER_ADMIN_ADDR=":9001" \
    WEBHOOK_RUNNER_LOG_FORMAT="text"

EXPOSE 9000 9001

# Run as root by default so we can talk to the bind-mounted Docker
# socket. Override with --user webhook + a properly-permissioned socket
# if your environment supports it.
ENTRYPOINT ["/usr/local/bin/webhook-runner"]
