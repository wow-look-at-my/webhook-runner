#!/bin/sh
# Deploy the frontend. A file, not a string in hook.json: the manifest may not
# carry a shell program (the runner and the schema both refuse nested command
# substitution in `command`), and this is what you get instead -- readable
# quoting and room for real error handling.
set -eu

target=$(jq -r '.deploy_target' "$HOOK_SETTINGS_FILE")
echo "deploying to ${target} with payload at ${HOOK_PAYLOAD_FILE}"
head -c 200 "$HOOK_PAYLOAD_FILE"
