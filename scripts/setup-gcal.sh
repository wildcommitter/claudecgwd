#!/usr/bin/env bash
# Create the Google Calendar venv (service-account client). Idempotent — safe to
# re-run. Used for host setup and inside the Docker image build.
#
# Env: GCAL_VENV (default ~/.local/share/assistant/gcal-venv), PYTHON (python3).

set -euo pipefail

VENV="${GCAL_VENV:-$HOME/.local/share/assistant/gcal-venv}"
PYTHON="${PYTHON:-python3}"

if [[ ! -x "$VENV/bin/python" ]]; then
  echo "==> creating venv at $VENV"
  "$PYTHON" -m venv "$VENV"
fi

echo "==> installing google calendar client"
"$VENV/bin/pip" install -q -U pip
"$VENV/bin/pip" install -q google-api-python-client google-auth

echo "done: gcal venv ready at $VENV"
