# CLI-contract tests for `webhook-runner validate` — the docker-free hooks-tree
# gate whose exit codes and messages the webhooks fleet repo's CI depends on.
#
# Commands exec the freshly built binary as
# "${GO_TOOLCHAIN_DATS_BUILD_DIR:-build}/webhook-runner" (go-toolchain's dats
# phase stages copies there, off PATH; standalone runs fall back to build/ —
# see dats/cli.dats and CLAUDE.md "CLI contract tests").
#
# Each test declares its own hooks tree inline via inputs.files; {inputs.<path>}
# expands to a fixture file's ABSOLUTE path, so a tree's root is recovered as
# "$(dirname "{inputs.<hook>/hook.json}")/.." (dats has no directory
# placeholder). How to run + assertion semantics: CLAUDE.md "CLI contract tests".

# SANDBOX OFF (dats `sandbox: false`). These commands need the HOST: they exec
# the binary go-toolchain's dats phase stages under $GO_TOOLCHAIN_DATS_BUILD_DIR,
# which is an os.MkdirTemp under /tmp — and dats' bwrap sandbox gives a command
# a fresh /tmp, so inside it that path does not exist and every test exits 127.
# Nothing here needs isolating anyway: docker-free, offline, secret-free tests
# of our own freshly built CLI (see CLAUDE.md "CLI contract tests").
sandbox: false

tests:
  - desc: valid legacy tree (JSONC comments allowed) validates with exit 0
    cmd: '"${GO_TOOLCHAIN_DATS_BUILD_DIR:-build}/webhook-runner" validate "$(dirname "{inputs.myhook/hook.json}")/.."'
    inputs:
      files:
        myhook/hook.json: |
          {
            // JSONC comments are allowed in hook.json
            "$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json",
            /* block comments too */
            "command": ["echo", "hi"]
          }
        myhook/Dockerfile: |
          FROM alpine
    exit: 0
    outputs:
      # Deliberately the suite's ONE positional (line-map regex) case: it pins
      # the output ORDER and that nothing else appears on stdout. Everything
      # else uses the substring-list form — don't spread this one.
      stdout:
        0: 'layout: legacy'
        1: 'ok  myhook \(whr-hook/myhook:'
        2: '1 hook\(s\) validated'

  - desc: the committed examples/hooks tree stays loader-valid
    # cwd is the invocation cwd (the repo root), so the committed examples are
    # reachable directly — this doubles as the drift gate for examples/hooks/.
    cmd: '"${GO_TOOLCHAIN_DATS_BUILD_DIR:-build}/webhook-runner" validate examples/hooks'
    exit: 0
    outputs:
      stdout:
        - "layout: legacy"
        - ok  run-tests
        - hook(s) validated

  - desc: valid src layout with concurrency group and manager validates with exit 0
    cmd: '"${GO_TOOLCHAIN_DATS_BUILD_DIR:-build}/webhook-runner" validate "$(dirname "{inputs.cfg/concurrency.json}")/.."'
    inputs:
      files:
        cfg/concurrency.json: |
          {"groups": {"g": {"limit": 2}}}
        src/hooks/alpha/hook.json: |
          {
            "$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json",
            "command": ["echo", "hi"],
            "concurrency_group": "g"
          }
        src/hooks/alpha/Dockerfile: |
          FROM alpine
        src/managers/boss/manager.json: |
          {
            "$schema": "https://sites.pazer.build/webhook-runner/branch/master/manager.schema.json",
            "command": ["echo", "hi"],
            "spawn_targets": ["alpha"]
          }
        src/managers/boss/Dockerfile: |
          FROM alpine
    exit: 0
    outputs:
      stdout:
        - "layout: src"
        - group g (limit 2)
        - "ok  alpha (whr-hook/alpha:"
        - "[group: g]"
        - "ok  boss (manager,"
        - "[spawns: alpha]"
        - 1 hook(s) + 1 manager(s) validated

  - desc: a tree with zero hooks fails loudly (never a silent empty fleet)
    cmd: '"${GO_TOOLCHAIN_DATS_BUILD_DIR:-build}/webhook-runner" validate "$(dirname "{inputs.README.md}")"'
    inputs:
      files:
        README.md: "no hook directories here\n"
    exit: 1
    outputs:
      stderr:
        - ERR no hooks loaded
        - one or more hooks failed validation

  - desc: a nonexistent hooks dir is a validation failure, not a crash
    cmd: '"${GO_TOOLCHAIN_DATS_BUILD_DIR:-build}/webhook-runner" validate /nonexistent/dats-contract-test'
    exit: 1
    outputs:
      stderr:
        - ERR read hooks dir
        - one or more hooks failed validation

  - desc: hook without a Dockerfile is rejected
    cmd: '"${GO_TOOLCHAIN_DATS_BUILD_DIR:-build}/webhook-runner" validate "$(dirname "{inputs.h/hook.json}")/.."'
    inputs:
      files:
        h/hook.json: |
          {"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json", "command": ["x"]}
    exit: 1
    outputs:
      stderr:
        - 'ERR hook "h": hook must ship a Dockerfile next to hook.json'

  - desc: hook.json without $schema is rejected
    cmd: '"${GO_TOOLCHAIN_DATS_BUILD_DIR:-build}/webhook-runner" validate "$(dirname "{inputs.h/hook.json}")/.."'
    inputs:
      files:
        h/hook.json: |
          {"command": ["x"]}
        h/Dockerfile: |
          FROM alpine
    exit: 1
    outputs:
      stderr:
        - 'ERR hook "h": $schema is required'

  - desc: unknown hook.json field is rejected (DisallowUnknownFields)
    cmd: '"${GO_TOOLCHAIN_DATS_BUILD_DIR:-build}/webhook-runner" validate "$(dirname "{inputs.h/hook.json}")/.."'
    inputs:
      files:
        h/hook.json: |
          {"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json", "command": ["x"], "image": "alpine"}
        h/Dockerfile: |
          FROM alpine
    exit: 1
    outputs:
      stderr:
        - 'ERR hook "h": decode hook.json:'
        - unknown field "image"

  - desc: referencing an undeclared concurrency group fails closed
    cmd: '"${GO_TOOLCHAIN_DATS_BUILD_DIR:-build}/webhook-runner" validate "$(dirname "{inputs.h/hook.json}")/.."'
    inputs:
      files:
        h/hook.json: |
          {"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json", "command": ["x"], "concurrency_group": "nope"}
        h/Dockerfile: |
          FROM alpine
    exit: 1
    outputs:
      stderr:
        - hook "h" references undeclared concurrency group "nope"

  - desc: skip_if with a non-compiling regex is a load error
    cmd: '"${GO_TOOLCHAIN_DATS_BUILD_DIR:-build}/webhook-runner" validate "$(dirname "{inputs.h/hook.json}")/.."'
    inputs:
      files:
        h/hook.json: |
          {"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json", "command": ["x"], "skip_if": [{"header:x-github-event": {"regex": "["}}]}
        h/Dockerfile: |
          FROM alpine
    exit: 1
    outputs:
      stderr:
        - 'skip_if[0] key "header:x-github-event": invalid regex'

  - desc: skip_if with an unknown operator is a load error
    cmd: '"${GO_TOOLCHAIN_DATS_BUILD_DIR:-build}/webhook-runner" validate "$(dirname "{inputs.h/hook.json}")/.."'
    inputs:
      files:
        h/hook.json: |
          {"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json", "command": ["x"], "skip_if": [{"action": {"frobnicate": "x"}}]}
        h/Dockerfile: |
          FROM alpine
    exit: 1
    outputs:
      stderr:
        - unknown operator "frobnicate"

  - desc: run_title with an unterminated placeholder is a load error
    cmd: '"${GO_TOOLCHAIN_DATS_BUILD_DIR:-build}/webhook-runner" validate "$(dirname "{inputs.h/hook.json}")/.."'
    inputs:
      files:
        h/hook.json: |
          {"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json", "command": ["x"], "run_title": "unterminated {{oops"}
        h/Dockerfile: |
          FROM alpine
    exit: 1
    outputs:
      stderr:
        - 'invalid run_title: unterminated "{{" placeholder'

  - desc: mixed layout (top-level hook dir beside src/hooks/) is a hard error
    cmd: '"${GO_TOOLCHAIN_DATS_BUILD_DIR:-build}/webhook-runner" validate "$(dirname "{inputs.leftover/hook.json}")/.."'
    inputs:
      files:
        src/hooks/alpha/hook.json: |
          {"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json", "command": ["x"]}
        src/hooks/alpha/Dockerfile: |
          FROM alpine
        leftover/hook.json: |
          {"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json", "command": ["x"]}
        leftover/Dockerfile: |
          FROM alpine
    exit: 1
    outputs:
      stdout:
        # the src hook itself still validates; only the stray dir is rejected
        - ok  alpha
      stderr:
        - "ERR mixed hook layout: top-level hook directory"
        - leftover

  - desc: manager spawn_targets naming an undeclared hook fails closed
    cmd: '"${GO_TOOLCHAIN_DATS_BUILD_DIR:-build}/webhook-runner" validate "$(dirname "{inputs.cfg/concurrency.json}")/.."'
    inputs:
      files:
        cfg/concurrency.json: |
          {"groups": {}}
        src/hooks/alpha/hook.json: |
          {"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json", "command": ["x"]}
        src/hooks/alpha/Dockerfile: |
          FROM alpine
        src/managers/boss/manager.json: |
          {"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json", "command": ["x"], "spawn_targets": ["ghost"]}
        src/managers/boss/Dockerfile: |
          FROM alpine
    exit: 1
    outputs:
      stderr:
        - 'ERR manager "boss": spawn_targets entry "ghost" does not name a declared hook'

  - desc: manager id colliding with a hook id is rejected (one namespace)
    cmd: '"${GO_TOOLCHAIN_DATS_BUILD_DIR:-build}/webhook-runner" validate "$(dirname "{inputs.cfg/concurrency.json}")/.."'
    inputs:
      files:
        cfg/concurrency.json: |
          {"groups": {}}
        src/hooks/dup/hook.json: |
          {"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json", "command": ["x"]}
        src/hooks/dup/Dockerfile: |
          FROM alpine
        src/managers/dup/manager.json: |
          {"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json", "command": ["x"]}
        src/managers/dup/Dockerfile: |
          FROM alpine
    exit: 1
    outputs:
      stderr:
        - 'ERR manager "dup": id collides with a hook of the same name'

  # The settings contract: a hook's OWN config is validated against the
  # settings.schema.json it ships, AT LOAD. These three cases are the whole
  # guarantee — wrong config never becomes a running hook, and config with no
  # contract is refused outright.
  - desc: settings that violate the hook's settings.schema.json fail validation
    cmd: '"${GO_TOOLCHAIN_DATS_BUILD_DIR:-build}/webhook-runner" validate "$(dirname "{inputs.myhook/hook.json}")/.."'
    inputs:
      files:
        myhook/hook.json: |
          {"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json", "command": ["x"], "settings": {"pacing_ms": "not-a-number"}}
        myhook/settings.schema.json: |
          {
            "type": "object",
            "required": ["app_id"],
            "properties": {"app_id": {"type": "string"}, "pacing_ms": {"type": "integer"}}
          }
        myhook/Dockerfile: |
          FROM alpine
    exit: 1
    outputs:
      stderr:
        - 'settings does not match settings.schema.json'

  - desc: a required setting left out fails validation (unconfigured is a load error)
    cmd: '"${GO_TOOLCHAIN_DATS_BUILD_DIR:-build}/webhook-runner" validate "$(dirname "{inputs.myhook/hook.json}")/.."'
    inputs:
      files:
        myhook/hook.json: |
          {"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json", "command": ["x"]}
        myhook/settings.schema.json: |
          {"type": "object", "required": ["app_id"], "properties": {"app_id": {"type": "string"}}}
        myhook/Dockerfile: |
          FROM alpine
    exit: 1
    outputs:
      stderr:
        - 'settings does not match settings.schema.json'

  - desc: settings declared without a settings.schema.json is refused
    cmd: '"${GO_TOOLCHAIN_DATS_BUILD_DIR:-build}/webhook-runner" validate "$(dirname "{inputs.myhook/hook.json}")/.."'
    inputs:
      files:
        myhook/hook.json: |
          {"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json", "command": ["x"], "settings": {"anything": 1}}
        myhook/Dockerfile: |
          FROM alpine
    exit: 1
    outputs:
      stderr:
        - 'settings is declared but settings.schema.json is missing'

  - desc: settings matching the schema validate with exit 0
    cmd: '"${GO_TOOLCHAIN_DATS_BUILD_DIR:-build}/webhook-runner" validate "$(dirname "{inputs.myhook/hook.json}")/.."'
    inputs:
      files:
        myhook/hook.json: |
          {"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json", "command": ["x"], "settings": {"app_id": "42", "pacing_ms": 1000}}
        myhook/settings.schema.json: |
          {
            "type": "object",
            "additionalProperties": false,
            "required": ["app_id"],
            "properties": {"app_id": {"type": "string"}, "pacing_ms": {"type": "integer"}}
          }
        myhook/Dockerfile: |
          FROM alpine
    exit: 0
    outputs:
      stdout:
        - ok  myhook

  - desc: the retired env block is rejected outright, never silently ignored
    cmd: '"${GO_TOOLCHAIN_DATS_BUILD_DIR:-build}/webhook-runner" validate "$(dirname "{inputs.myhook/hook.json}")/.."'
    inputs:
      files:
        myhook/hook.json: |
          {"$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json", "command": ["x"], "env": {"TOKEN": "abc"}}
        myhook/Dockerfile: |
          FROM alpine
    exit: 1
    outputs:
      stderr:
        - 'unknown field "env"'

  # The PUBLISHED schema is enforced at load now, by the same implementation
  # (wow-look-at-my/json-validator) the hooks repo runs in CI -- so "passes CI"
  # and "loads at runtime" stop being two different questions. These cases cover
  # what the Go model cannot express.
  - desc: a manifest violating the published schema fails validation
    cmd: '"${GO_TOOLCHAIN_DATS_BUILD_DIR:-build}/webhook-runner" validate "$(dirname "{inputs.myhook/hook.json}")/.."'
    inputs:
      files:
        myhook/hook.json: |
          {
            "$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json",
            "command": ["x"],
            "github_status": {"enabled": true, "context": "ci", "target_url": "not a url"}
          }
        myhook/Dockerfile: |
          FROM alpine
    exit: 1
    outputs:
      stderr:
        - 'does not match the published schema'
        - target_url

  - desc: a $schema that is not a URI is rejected at load
    cmd: '"${GO_TOOLCHAIN_DATS_BUILD_DIR:-build}/webhook-runner" validate "$(dirname "{inputs.myhook/hook.json}")/.."'
    inputs:
      files:
        myhook/hook.json: |
          {"$schema": "s", "command": ["x"]}
        myhook/Dockerfile: |
          FROM alpine
    exit: 1
    outputs:
      stderr:
        - 'does not match the published schema'

  # A manifest is argv, NOT a place to write shell. Nested command/process
  # substitution is refused at load AND by the published schema (the same rule
  # the hooks repo's CI enforces), because a shell program inside a JSON string
  # is escaped twice, checkable by nothing, and testable only in production.
  - desc: command substitution in a hook's command is a load error
    cmd: '"${GO_TOOLCHAIN_DATS_BUILD_DIR:-build}/webhook-runner" validate "$(dirname "{inputs.myhook/hook.json}")/.."'
    inputs:
      files:
        myhook/hook.json: |
          {
            "$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json",
            "command": ["sh", "-c", "curl --data-binary @$HOOK_PAYLOAD_FILE \"$(sed -n 's/x/y/p' $HOOK_SETTINGS_FILE)\""]
          }
        myhook/Dockerfile: |
          FROM alpine
    exit: 1
    outputs:
      stderr:
        - 'must not carry a shell program'
        - '.sh file'

  - desc: backticks are refused the same way
    cmd: '"${GO_TOOLCHAIN_DATS_BUILD_DIR:-build}/webhook-runner" validate "$(dirname "{inputs.myhook/hook.json}")/.."'
    inputs:
      files:
        myhook/hook.json: |
          {
            "$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json",
            "command": ["sh", "-c", "echo `date`"]
          }
        myhook/Dockerfile: |
          FROM alpine
    exit: 1
    outputs:
      stderr:
        - 'must not carry a shell program'

  - desc: a plain $VAR reference stays legal (that is what those vars are for)
    cmd: '"${GO_TOOLCHAIN_DATS_BUILD_DIR:-build}/webhook-runner" validate "$(dirname "{inputs.myhook/hook.json}")/.."'
    inputs:
      files:
        myhook/hook.json: |
          {
            "$schema": "https://sites.pazer.build/webhook-runner/branch/master/hook.schema.json",
            "command": ["sh", "-c", "cat $HOOK_PAYLOAD_FILE"]
          }
        myhook/Dockerfile: |
          FROM alpine
    exit: 0
    outputs:
      stdout:
        - ok  myhook
