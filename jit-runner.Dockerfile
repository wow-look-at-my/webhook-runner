# syntax=docker/dockerfile:1.7
#
# JIT GitHub Actions runner image.
# Contains the jit-runner handler binary and the GitHub Actions runner.
# Rebuild with --build-arg RUNNER_VERSION=X.Y.Z to update the runner.

FROM golang:1.24-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS="-trimpath"
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -ldflags="-s -w" -o /out/jit-runner ./cmd/jit-runner

FROM ubuntu:24.04

ARG RUNNER_VERSION=2.322.0
ARG RUNNER_ARCH=x64

RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates curl libicu74 && \
    rm -rf /var/lib/apt/lists/*

RUN useradd -m runner

WORKDIR /opt/actions-runner
RUN curl -fsSL \
      "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-${RUNNER_ARCH}-${RUNNER_VERSION}.tar.gz" \
    | tar xz && \
    chown -R runner:runner .

COPY --from=build /out/jit-runner /usr/local/bin/jit-runner

USER runner
ENV RUNNER_DIR=/opt/actions-runner
