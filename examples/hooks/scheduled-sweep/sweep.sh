#!/bin/sh
set -eu
echo "scheduled sweep at $(date -u +%FT%TZ)"
cat "$HOOK_PAYLOAD_FILE"
