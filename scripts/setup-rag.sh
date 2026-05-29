#!/usr/bin/env bash
# Create the RAG venv (local embeddings) and pre-download the model. Idempotent
# — safe to re-run. Used both for host setup and inside the Docker image build.
#
# Env: RAG_VENV (default ~/.local/share/assistant/rag-venv),
#      RAG_MODEL (default BAAI/bge-small-en-v1.5), PYTHON (default python3).

set -euo pipefail

VENV="${RAG_VENV:-$HOME/.local/share/assistant/rag-venv}"
MODEL="${RAG_MODEL:-BAAI/bge-small-en-v1.5}"
PYTHON="${PYTHON:-python3}"

if [[ ! -x "$VENV/bin/python" ]]; then
  echo "==> creating venv at $VENV"
  "$PYTHON" -m venv "$VENV"
fi

echo "==> installing fastembed + extractors"
"$VENV/bin/pip" install -q -U pip
# fastembed: ONNX embeddings (no torch). pypdf/python-docx: text extraction.
"$VENV/bin/pip" install -q fastembed pypdf python-docx numpy

echo "==> pre-downloading embedding model: $MODEL"
"$VENV/bin/python" - "$MODEL" <<'PY'
import sys
from fastembed import TextEmbedding
m = TextEmbedding(model_name=sys.argv[1])
list(m.embed(["warm up the model cache"]))
print("model ready:", sys.argv[1])
PY

echo "done: RAG venv ready at $VENV (model $MODEL)"
