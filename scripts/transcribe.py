#!/usr/bin/env python3
"""Transcribe an audio file to text with faster-whisper; print the transcript.

Usage: transcribe.py <audio-file> [model]

Run with the STT venv's interpreter
(~/.local/share/assistant/stt-venv/bin/python). The model is downloaded and
cached on first use. faster-whisper decodes the audio itself (via PyAV/ffmpeg),
so OGG/Opus voice notes work without manual conversion.
"""
import sys


def main() -> int:
    if len(sys.argv) < 2:
        print("usage: transcribe.py <audio-file> [model]", file=sys.stderr)
        return 2
    audio = sys.argv[1]
    model_name = sys.argv[2] if len(sys.argv) > 2 else "small"

    from faster_whisper import WhisperModel

    model = WhisperModel(model_name, device="cpu", compute_type="int8")
    segments, _info = model.transcribe(audio, vad_filter=True)
    text = " ".join(seg.text.strip() for seg in segments).strip()
    sys.stdout.write(text)
    return 0


if __name__ == "__main__":
    sys.exit(main())
