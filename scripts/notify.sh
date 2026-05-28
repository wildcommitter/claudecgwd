#!/usr/bin/env bash
# Push a proactive notification to every configured chat surface (Telegram +
# WhatsApp) via the bridge's notify FIFO. The assistant calls this when a
# background watcher/build finishes, since a proactive ping has no inbound turn
# to ride on.
#
#   scripts/notify.sh "✅ build done: ..."
#
# The message is base64-encoded onto a single FIFO line so multi-line text
# survives. The running bridge decodes and fans it out.

set -euo pipefail

fifo="${CLAUDECGWD_NOTIFY_FIFO:-$HOME/.local/share/assistant/notify.fifo}"
msg="$*"

if [[ -z "$msg" ]]; then
  echo "usage: notify.sh <message>" >&2
  exit 2
fi
if [[ ! -p "$fifo" ]]; then
  echo "notify FIFO not present (is the bridge running?): $fifo" >&2
  exit 1
fi

printf '%s\n' "$(printf '%s' "$msg" | base64 | tr -d '\n')" >> "$fifo"
