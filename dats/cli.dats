# CLI-contract tests for webhook-runner's general command-line surface:
# version, help, argument/flag errors, and the docker-free `test` paths.
# How to run + assertion semantics: CLAUDE.md "CLI contract tests".
#
# Commands exec the freshly built binary as
# "${GO_TOOLCHAIN_DATS_BUILD_DIR:-build}/webhook-runner": go-toolchain's dats
# phase stages throwaway copies under $GO_TOOLCHAIN_DATS_BUILD_DIR (and does
# NOT put them on PATH — a bare `webhook-runner` exits 127 there), while a
# standalone `dats test dats` from the repo root falls back to build/.
#
# NOTE: never pass the binary a bare unrecognized word — it STARTS THE SERVER
# (the root command's [hooks-dir] positional); see CLAUDE.md. Tests near the
# serve path must error before binding and carry a timeout as a hang guard.

# These suites run the repo's own freshly built binary against fixtures in a
# temp tree, and are kept docker-free and offline so they pass on a bare
# runner (CLAUDE.md, "CLI contract tests"). dats sandboxes commands by
# default via bubblewrap or docker; the org's default CI runner has neither,
# so opt out here rather than make a bare runner a lie. Drop this line if the
# runner ever grows bubblewrap.
sandbox: false

tests:
	- desc: version prints a version string and exits 0
	  cmd: '"${GO_TOOLCHAIN_DATS_BUILD_DIR:-build}/webhook-runner" version'
	  exit: 0
	  outputs:
		# line 0 must look like a version: dev, dev-<sha>, or v<pseudo-version>
		stdout:
			0: (dev|v[0-9])

	- desc: root --help lists the public subcommands and exits 0
	  cmd: '"${GO_TOOLCHAIN_DATS_BUILD_DIR:-build}/webhook-runner" --help'
	  exit: 0
	  outputs:
		stdout:
			- Available Commands
			- validate
			- test
			- version

	- desc: validate --help documents the hooks-dir argument
	  cmd: '"${GO_TOOLCHAIN_DATS_BUILD_DIR:-build}/webhook-runner" validate --help'
	  exit: 0
	  outputs:
		stdout:
			- webhook-runner validate <hooks-dir>
	- desc: an unknown flag is an error, not a server start
	  cmd: '"${GO_TOOLCHAIN_DATS_BUILD_DIR:-build}/webhook-runner" --frobnicate'
	  timeout: 30s
	  exit: 1
	  outputs:
		stderr:
			- "unknown flag: --frobnicate"

	- desc: validate without its hooks-dir argument is an argument error
	  cmd: '"${GO_TOOLCHAIN_DATS_BUILD_DIR:-build}/webhook-runner" validate'
	  exit: 1
	  outputs:
		stderr:
			- accepts 1 arg(s), received 0

	- desc: serve without a hooks dir or repo fails before binding anything
	  # env -u makes the test hermetic even if the host exports these vars;
	  # the timeout is the hang guard should this ever regress into serving.
	  cmd: 'env -u WEBHOOK_RUNNER_HOOKS_DIR -u WEBHOOK_RUNNER_HOOKS_REPO "${GO_TOOLCHAIN_DATS_BUILD_DIR:-build}/webhook-runner"'
	  timeout: 30s
	  exit: 1
	  outputs:
		stderr:
			- hooks directory required

	- desc: test on a valid tree with no declared tests reports and exits 0 (docker-free)
	  cmd: '"${GO_TOOLCHAIN_DATS_BUILD_DIR:-build}/webhook-runner" test "$(dirname "{inputs.h/hook.json}")/.."'
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
	  cmd: '"${GO_TOOLCHAIN_DATS_BUILD_DIR:-build}/webhook-runner" test "$(dirname "{inputs.h/hook.json}")/.."'
	  inputs:
		files:
			h/hook.json: |
				{"$schema": "s", "command": ["x"]}
	  exit: 1
	  outputs:
		stderr:
			- one or more hooks failed validation
