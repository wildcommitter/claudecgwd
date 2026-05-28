#!/usr/bin/env bash
# Create the faster-whisper venv and pre-download the model. Idempotent — safe
# to re-run. Used both for host setup and inside the Docker image build.
#
# Env: STT_VENV (default ~/.local/share/assistant/stt-venv),
#      WHISPER_MODEL (default small), PYTHON (default python3).

set -euo pipefail

VENV="${STT_VENV:-$HOME/.local/share/assistant/stt-venv}"
MODEL="${WHISPER_MODEL:-small}"
PYTHON="${PYTHON:-python3}"

if [[ ! -x "$VENV/bin/python" ]]; then
  echo "==> creating venv at $VENV"
  "$PYTHON" -m venv "$VENV"
fi

echo "==> installing faster-whisper"
"$VENV/bin/pip" install -q -U pip
"$VENV/bin/pip" install -q faster-whisper

echo "==> pre-downloading model: $MODEL"
"$VENV/bin/python" - "$MODEL" <<'PY'
import sys
from faster_whisper import WhisperModel
WhisperModel(sys.argv[1], device="cpu", compute_type="int8")
print("model ready:", sys.argv[1])
PY

echo "done: STT venv ready at $VENV (model $MODEL)"
