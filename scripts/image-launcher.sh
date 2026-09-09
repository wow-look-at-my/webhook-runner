#!/bin/sh
# HACK, pending the real fix. The image installs this at
# /usr/local/bin/webhook-runner, and the APE beside it under /usr/local/lib.
# Running the APE through the shell keeps every spelling of the entrypoint
# working, including the one an older container recorded.
exec /bin/sh /usr/local/lib/webhook-runner/webhook-runner "$@"
