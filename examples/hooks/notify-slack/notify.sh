#!/bin/sh
# Forward the delivery to Slack.
#
# This is a FILE, not a string in hook.json, because a manifest must not carry
# a shell program: the runner and the published schema both reject nested
# command substitution in `command` (see the runner's shellsafe.go). What the
# rule buys you is visible below -- one level of quoting, a real failure when
# the webhook URL is missing, and something you can shellcheck and run by hand.
set -eu

url=$(jq -r '.slack_webhook_url' "$HOOK_SETTINGS_FILE")
if [ -z "$url" ] || [ "$url" = "null" ]; then
  echo "notify-slack: settings.slack_webhook_url is empty" >&2
  exit 1
fi

curl -fsS -X POST -H 'content-type: application/json' \
  --data-binary "@$HOOK_PAYLOAD_FILE" "$url"
