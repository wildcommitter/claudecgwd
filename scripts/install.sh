#!/usr/bin/env bash
# Build, install, and reload the assistant.
#
# This is the canonical build path for this project — use it instead of bare
# `go build`. The post-commit git hook also calls it.
#
#   ./scripts/install.sh              # build + install + restart
#   ./scripts/install.sh --no-restart # build + install only

set -euo pipefail
cd "$(dirname "$0")/.."

RESTART=1
for arg in "$@"; do
  case "$arg" in
    --no-restart) RESTART=0 ;;
    *) echo "unknown arg: $arg" >&2; exit 2 ;;
  esac
done

# Make sure go is on PATH (handles the case where the script is invoked from
# a non-interactive shell — e.g. a git hook — that didn't source .zshrc).
export PATH="$HOME/.local/go/bin:$PATH"

DEST="$HOME/.local/bin/assistant"
mkdir -p "$(dirname "$DEST")"

# Ensure the post-commit hook is installed (idempotent — safe on every run).
HOOK_SRC="scripts/post-commit.hook"
HOOK_DST=".git/hooks/post-commit"
if [[ -d .git && -f "$HOOK_SRC" ]] && ! cmp -s "$HOOK_SRC" "$HOOK_DST" 2>/dev/null; then
  echo "==> installing $HOOK_DST"
  cp "$HOOK_SRC" "$HOOK_DST"
  chmod +x "$HOOK_DST"
fi

echo "==> go test ./..."
go test ./...

echo "==> go build -> $DEST"
go build -o "$DEST" ./cmd/assistant

if [[ "$RESTART" == 1 ]]; then
  if systemctl --user is-enabled --quiet assistant 2>/dev/null; then
    echo "==> systemctl --user restart assistant"
    systemctl --user restart assistant
    sleep 1
    systemctl --user is-active --quiet assistant \
      && echo "    active" \
      || { echo "    NOT ACTIVE"; systemctl --user status assistant --no-pager | head -10; exit 1; }
  else
    echo "==> systemd unit not enabled, skipping restart"
  fi
fi

echo "done."
