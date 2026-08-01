#!/bin/sh
# The settings document reaches the container as a file; printing it here (from
# a script, not from a manifest string) is what the e2e asserts.
set -eu
echo "id=$HOOK_ID custom=$(cat "$HOOK_SETTINGS_FILE")"
