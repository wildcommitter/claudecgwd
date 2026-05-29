# Running the assistant in Docker (sandboxed)

This packages the bridge + a pinned `claude` binary into one image so the
bypass-permissions Claude session runs inside a container that can see **only**
the paths you mount. The goals are **sandboxing** (bound the blast radius of an
agent running with `--permission-mode bypassPermissions`) and **portability**
(a self-contained image that runs anywhere Docker does).

## What's in the image

- `debian:bookworm-slim` (glibc — required by the native `claude` binary).
- The Go `assistant` bridge, built in a first stage.
- `claude` `2.1.154`, installed via the official native installer and pinned;
  auto-update is disabled (`DISABLE_AUTOUPDATER=1`) so the image is the unit of
  update. Bump with `--build-arg CLAUDE_VERSION=x.y.z`.
- **Voice transcription bundled**: `python3` + `ffmpeg` + a faster-whisper venv
  with the model baked in at build (`scripts/setup-stt.sh`, `--build-arg
  WHISPER_MODEL=small`). So STT works in the container with no host dependency
  — but it adds ~600 MB to the image. Keep `WHISPER_MODEL` in sync with
  `stt.model` in config.
- **RAG search bundled**: a second venv with local (ONNX/fastembed) embeddings
  and the model baked in (`scripts/setup-rag.sh`, `--build-arg
  RAG_MODEL=BAAI/bge-small-en-v1.5`). Powers `/search` and the `rag-search`
  skill over attachments + conversations — no host dependency, nothing leaves
  the box. The index lives under the mounted store volume, so it persists.
- Runs as uid/gid `1000` (matching the host user) so bind-mounted files keep
  their ownership.

The container mirrors the host layout — `HOME=/home/user`, `claude` at
`~/.local/bin/claude`, workdir `/home/user/claudecgwd` — so your existing
`config.yaml` (which references those absolute paths) and the pinned
`--session-id` transcript keep resolving unchanged.

## The sandbox boundary

Only these are mounted (everything else of the host is invisible):

| Host path                     | Container path                  | Mode | Why |
|-------------------------------|----------------------------------|------|-----|
| `~/.claude`                   | `/home/user/.claude`             | rw   | auth (`.credentials.json`) + session transcripts |
| `~/.claude.json`              | `/home/user/.claude.json`        | rw   | user/auth state |
| `~/.config/assistant`         | `/home/user/.config/assistant`   | ro   | `config.yaml` + `secrets.env` |
| `~/claudecgwd`                | `/home/user/claudecgwd`          | rw   | the project workdir (the only host code the agent can touch) |
| `assistant-state` (named vol) | `/home/user/.local/share/assistant` | rw | persistent runtime state (see below) |

To let the assistant work on more projects, add more `volumes:` entries in
`docker-compose.yml`. That is the knob that trades sandbox tightness for reach.

### Persistent state

Runtime state — the inbox, RAG index, WhatsApp pairing (`whatsapp.db`),
reminders, routines, the project registry, and on-demand TTS voices — lives
under `/home/user/.local/share/assistant`. It's a **named volume**, not a bind
mount, on purpose: the image bakes the STT/RAG/TTS venvs and the default voice
into that same directory, and a named volume is auto-populated from the image on
first run, whereas a bind mount of an empty host dir would shadow (hide) them and
break voice/search. The trade-off: after rebuilding the image with updated
venvs/models, recreate the volume to pick them up —
`docker volume rm claudecgwd_assistant-state` (or `podman volume rm
assistant-state` for the Quadlet unit).

Hardening: `no-new-privileges`, `cap_drop: ALL`, and a `tmpfs` `/tmp`.

## Podman (rootless) — the recommended runtime

This is built for **rootless Podman** (5.x). Podman reads the `Dockerfile`
as-is, and `alias docker=podman` makes the `docker …` commands below work in
your shell. Two Podman specifics matter:

- **`keep-id` user namespace.** Rootless Podman maps your host user to
  *container-root* by default, which would leave the bind-mounted `~/.claude`
  owned by a high subuid inside the container — and `claude` couldn't read its
  credentials. The compose file and Quadlet unit set `keep-id` so your host uid
  stays identical inside. (A bare `alias docker=podman` does **not** fix this.)
- **No daemon / systemd aliases.** systemd units don't read shell aliases, so
  the unit files call `podman` directly.

## Run it

> **Stop the native service first.** Running both means two bots polling the
> same Telegram token and two writers on the same session transcript.
>
> ```sh
> systemctl --user stop assistant
> ```

```sh
cd ~/claudecgwd
podman build -t claudecgwd-assistant:latest .   # build the image

# With a compose provider (podman-compose / docker-compose) installed:
podman compose up --build        # foreground (watch logs)
podman compose up -d --build     # detached
podman compose down              # stop
```

If you don't have a compose provider, use the Quadlet unit below instead — it
needs no compose at all.

## Run it as a service

**Recommended — Quadlet** (`deploy/assistant.container`): native Podman+systemd,
no compose provider required. Build the image, drop the `.container` file into
`~/.config/containers/systemd/`, `daemon-reload`, and `systemctl --user start
assistant`. Full steps are in the file's header.

**Alternative — compose unit** (`deploy/assistant-docker.service`): wraps
`podman compose up --build`; requires a compose provider on PATH.

## Notes & caveats

- **Auth**: this uses your existing OAuth login by bind-mounting `~/.claude`
  (chosen over an API key to keep your current session). If you ever
  re-authenticate, do it on the host (or `docker compose exec assistant claude`)
  so the creds land in the shared volume.
- **Shared `~/.claude` across versions**: if the host's native `claude`
  auto-updates past `2.1.154`, the host and container run different binaries
  over the same `~/.claude`. Fine in practice, but pick one to actually run.
- **The post-commit `install.sh` hook** still builds/restarts the *native*
  binary. Under Podman you rebuild the image instead (`podman build` /
  `up --build`); the Quadlet and compose units rebuild on (re)start.
- The likeliest first-build wrinkles to watch: the `golang:1.26-bookworm` base
  tag must exist for your Go version, and the `claude` installer must succeed
  non-interactively in the container (the Dockerfile runs `claude --version` to
  fail fast if it didn't).
