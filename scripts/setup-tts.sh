#!/usr/bin/env bash
# Create the TTS venv (local piper) and download the voice model. Idempotent —
# safe to re-run. Used both for host setup and inside the Docker image build.
#
# Env: TTS_VENV (default ~/.local/share/assistant/tts-venv),
#      TTS_VOICE (default en_US-amy-medium), TTS_VOICES_DIR
#      (default ~/.local/share/assistant/tts-voices), PYTHON (default python3).

set -euo pipefail

VENV="${TTS_VENV:-$HOME/.local/share/assistant/tts-venv}"
VOICE="${TTS_VOICE:-en_US-amy-medium}"
VOICES_DIR="${TTS_VOICES_DIR:-$HOME/.local/share/assistant/tts-voices}"
PYTHON="${PYTHON:-python3}"

if [[ ! -x "$VENV/bin/python" ]]; then
  echo "==> creating venv at $VENV"
  "$PYTHON" -m venv "$VENV"
fi

echo "==> installing piper-tts"
"$VENV/bin/pip" install -q -U pip
"$VENV/bin/pip" install -q piper-tts

# A voice name like en_US-amy-medium maps to the piper-voices repo path
#   <lang>/<region>/<name>/<quality>/<voice>.onnx(.json)
mkdir -p "$VOICES_DIR"
lang="${VOICE%%_*}"        # en
region="${VOICE%%-*}"      # en_US
rest="${VOICE#*-}"         # amy-medium
name="${rest%%-*}"         # amy
quality="${rest#*-}"       # medium
base="https://huggingface.co/rhasspy/piper-voices/resolve/main/${lang}/${region}/${name}/${quality}/${VOICE}"

for ext in onnx onnx.json; do
  if [[ ! -f "$VOICES_DIR/$VOICE.$ext" ]]; then
    echo "==> downloading $VOICE.$ext"
    curl -fsSL "$base.$ext" -o "$VOICES_DIR/$VOICE.$ext"
  fi
done

echo "==> warming up piper"
echo "ready" | "$VENV/bin/piper" --model "$VOICES_DIR/$VOICE.onnx" --output_file /tmp/tts-warmup.wav
rm -f /tmp/tts-warmup.wav

echo "done: TTS venv ready at $VENV (voice $VOICE)"
