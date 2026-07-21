# CLI-contract tests for webhook-runner's general command-line surface:
# version, help, argument/flag errors, and the docker-free `test` paths.
#
# Run from the repo root with the built binary on PATH:
#
#   PATH="$PWD/build:$PATH" dats test dats
#
# NOTE: never invoke a bare `webhook-runner <word>` here — the root command
# takes [hooks-dir] as a positional, so an unrecognized bare word is treated
# as a hooks dir and STARTS THE SERVER (binding :9000/:9001). Every test in
# this file either uses a subcommand, errors before serving, or carries a
# timeout as a hang guard.

tests:
  - desc: version prints a version string and exits 0
    cmd: webhook-runner version
    exit: 0
    outputs:
      # dev / dev-<sha> / v<pseudo-version> — all carry a "v"
      stdout:
        - v

  - desc: root --help lists the public subcommands and exits 0
    cmd: webhook-runner --help
    exit: 0
    outputs:
      stdout:
        - Available Commands
        - validate
        - test
        - version

  - desc: validate --help documents the hooks-dir argument
    cmd: webhook-runner validate --help
    exit: 0
    outputs:
      stdout:
        - webhook-runner validate <hooks-dir>

  - desc: an unknown flag is an error, not a server start
    cmd: webhook-runner --frobnicate
    timeout: 30s
    exit: 1
    outputs:
      stderr:
        - "unknown flag: --frobnicate"

  - desc: validate without its hooks-dir argument is an argument error
    cmd: webhook-runner validate
    exit: 1
    outputs:
      stderr:
        - accepts 1 arg(s), received 0

  - desc: serve without a hooks dir or repo fails before binding anything
    # env -u makes the test hermetic even if the host exports these vars;
    # the timeout is the hang guard should this ever regress into serving.
    cmd: env -u WEBHOOK_RUNNER_HOOKS_DIR -u WEBHOOK_RUNNER_HOOKS_REPO webhook-runner
    timeout: 30s
    exit: 1
    outputs:
      stderr:
        - hooks directory required (positional arg, WEBHOOK_RUNNER_HOOKS_DIR, or WEBHOOK_RUNNER_HOOKS_REPO)

  - desc: test on a valid tree with no declared tests reports and exits 0 (docker-free)
    cmd: sh -c 'webhook-runner test "$(dirname "{inputs.h/hook.json}")/.."'
    inputs:
      files:
        h/hook.json: |
          {"$schema": "s", "command": ["echo", "hi"]}
        h/Dockerfile: |
          FROM alpine
    exit: 0
    outputs:
      stdout:
        - no hooks declare tests

  - desc: test on a tree that fails to load exits non-zero before any docker use
    cmd: sh -c 'webhook-runner test "$(dirname "{inputs.h/hook.json}")/.."'
    inputs:
      files:
        h/hook.json: |
          {"$schema": "s", "command": ["x"]}
    exit: 1
    outputs:
      stderr:
        - hook must ship a Dockerfile
        - one or more hooks failed validation
