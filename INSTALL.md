# Install & configure (Podman or Docker)

This guide takes you from nothing to a running, sandboxed assistant bridge in a
container. It covers **both Podman and Docker** and calls out — in
`Podman` / `Docker` boxes — every place the two diverge.

> **TL;DR of the difference:** the image and the compose file are the same for
> both runtimes. Only three things change between them: the **command name**
> (`podman` vs `docker`), how the container gets your **file ownership** on the
> bind mounts (`--userns=keep-id` vs `--user 1000:1000`), and how you run it as
> a **service** (Podman Quadlet vs a Docker/systemd unit). Everything else is
> identical.
>
> **Recommended runtime: rootless Podman.** This project is built and tested for
> it. Docker works too; the Docker-specific deltas are noted throughout.

For the deeper sandbox-boundary rationale, the persistent-state design, and the
Quadlet unit, see [docs/DOCKER.md](docs/DOCKER.md). This file is the
step-by-step setup.

---

## 1. Prerequisites

- **A container runtime** — one of:
  - **Podman** 4.x or 5.x (rootless). Check: `podman version`.
  - **Docker** 24+ with the Compose plugin. Check: `docker version`.
- **A Claude login.** The container reuses your host's `~/.claude` OAuth
  session (bind-mounted), so log in once on the host first:
  ```sh
  claude   # complete the login, then quit
  ```
  This must produce `~/.claude/.credentials.json`.
- **A Telegram bot token** (see §3) — the primary chat surface.
- **`git`** and this repo checked out at `~/claudecgwd` (the container mirrors
  that path, so keep it there unless you also edit the mounts).

> `Podman` — **btrfs hosts only:** rootless Podman's default `overlay` storage
> driver fails on btrfs. Set the native driver once:
> ```sh
> mkdir -p ~/.config/containers
> printf '[storage]\ndriver = "btrfs"\n' > ~/.config/containers/storage.conf
> ```

> `Podman` — make the `docker …` commands in this guide work verbatim:
> ```sh
> alias docker=podman
> ```
> This only affects your **interactive shell**. systemd units do **not** read
> shell aliases — the service files call `podman` directly (see §7).

---

## 2. Get the image

Two options. Pick one.

### Option A — pull the published image (fastest)

The CI publishes a ready-built image to GitHub Container Registry on every push
to `main`:

```sh
# Podman
podman pull ghcr.io/wildcommitter/claudecgwd:latest
# Docker
docker pull ghcr.io/wildcommitter/claudecgwd:latest
```

The package is public, so no `login` is needed. (If you ever make it private:
`podman login ghcr.io` / `docker login ghcr.io` with a GitHub PAT that has
`read:packages`.)

Available tags: `latest` and `main` (trunk), `sha-<commit>` (immutable, pin
this for reproducibility), and `X.Y.Z` for tagged releases.

### Option B — build it locally

Build from the working tree (picks up your local changes):

```sh
cd ~/claudecgwd
# Podman
podman build -t claudecgwd-assistant:latest .
# Docker
docker build -t claudecgwd-assistant:latest .
```

The build bakes in: the Go bridge, a **pinned `claude`** (`2.1.154`; bump with
`--build-arg CLAUDE_VERSION=x.y.z`), and the local CPU engines for voice
(faster-whisper STT + piper TTS), semantic search (fastembed RAG), and Google
Calendar — so those features work in the container with no host dependency.
This is a ~2 GB image and the first build is slow.

---

## 3. Configure

The container reads two files from `~/.config/assistant`, mounted read-only.

```sh
mkdir -p ~/.config/assistant

# Main config
cp config.example.yaml ~/.config/assistant/config.yaml

# Secrets (token lives here, NEVER in config.yaml or git)
cp deploy/secrets.env.example ~/.config/assistant/secrets.env
chmod 600 ~/.config/assistant/secrets.env
```

### `secrets.env`

```ini
TELEGRAM_BOT_TOKEN=123456:your-real-bot-token
```

Get the token from [@BotFather](https://t.me/BotFather) (`/newbot`).

### `config.yaml`

The example is pre-filled for the container layout (`claude` at
`/home/user/.local/bin/claude`, workdir `/home/user/claudecgwd`). You must set:

```yaml
claude:
  session_id: REPLACE-WITH-UUID    # `uuidgen` once; a stable id = your
                                   # conversation survives restarts

telegram:
  token_env: TELEGRAM_BOT_TOKEN    # name of the env var in secrets.env
  allowed_user_ids:
    - 123456789                    # YOUR numeric id — message @userinfobot
```

Generate the session id:

```sh
uuidgen
```

Voice (STT/TTS) and RAG are bundled in the image, so you can leave
`stt.enabled: true` / `tts.enabled: true` on. The audio language defaults to
`auto` (transcription auto-detects; spoken replies follow the language Claude
answers in). See §6 for the optional surfaces.

---

## 4. Run it (one-off / foreground)

> **Stop the native (non-container) service first if you have one** — two
> processes on the same Telegram token and the same session transcript will
> fight:
> ```sh
> systemctl --user stop assistant 2>/dev/null
> ```

Run the container directly. **This is the one command that differs between the
two runtimes** — the `--userns` / `--user` line:

> `Podman` (rootless):
> ```sh
> podman run -d --name claude-assistant \
>   --userns=keep-id \
>   --env-file ~/.config/assistant/secrets.env \
>   -e HOME=/home/user -e DISABLE_AUTOUPDATER=1 \
>   -v ~/.claude:/home/user/.claude \
>   -v ~/.claude.json:/home/user/.claude.json \
>   -v ~/.config/assistant:/home/user/.config/assistant:ro \
>   -v ~/claudecgwd:/home/user/claudecgwd \
>   -v assistant-state:/home/user/.local/share/assistant \
>   --security-opt no-new-privileges:true --cap-drop ALL --tmpfs /tmp \
>   ghcr.io/wildcommitter/claudecgwd:latest
> ```

> `Docker`:
> ```sh
> docker run -d --name claude-assistant \
>   --user 1000:1000 \
>   --env-file ~/.config/assistant/secrets.env \
>   -e HOME=/home/user -e DISABLE_AUTOUPDATER=1 \
>   -v ~/.claude:/home/user/.claude \
>   -v ~/.claude.json:/home/user/.claude.json \
>   -v ~/.config/assistant:/home/user/.config/assistant:ro \
>   -v ~/claudecgwd:/home/user/claudecgwd \
>   -v assistant-state:/home/user/.local/share/assistant \
>   --security-opt no-new-privileges:true --cap-drop ALL --tmpfs /tmp \
>   claudecgwd-assistant:latest   # local build; or ghcr.io/wildcommitter/claudecgwd:latest if pulled
> ```

**Why the difference?** Rootless Podman maps your host user to *container-root*
by default, which would leave the bind-mounted `~/.claude` owned by a high
subuid inside the container — and `claude` couldn't read its own credentials.
`--userns=keep-id` keeps your uid identical inside. Docker has no `keep-id`; the
image's user is uid `1000`, so `--user 1000:1000` matches your host uid
(assuming you're uid 1000 — check with `id -u`).

Follow logs / stop:

```sh
podman logs -f claude-assistant      # docker logs -f claude-assistant
podman rm -f claude-assistant        # docker rm -f claude-assistant
```

Then message your bot on Telegram. Send `/help` to see the commands.

---

## 5. Run it with Compose

A [`docker-compose.yml`](docker-compose.yml) is included. It **builds locally**
by default (Option B) and sets all the mounts and hardening for you.

> `Podman` — needs a compose provider (`podman-compose` **or** the
> `docker-compose` binary on PATH). The compose file already sets
> `userns_mode: "keep-id"`, so no edits are needed:
> ```sh
> cd ~/claudecgwd
> podman compose up --build       # foreground
> podman compose up -d --build    # detached
> podman compose logs -f
> podman compose down
> ```

> `Docker` — **edit `docker-compose.yml` first:** delete the
> `userns_mode: "keep-id"` line (Docker has no keep-id) and add `user:
> "1000:1000"` to the `assistant:` service. Then:
> ```sh
> cd ~/claudecgwd
> docker compose up -d --build
> docker compose logs -f
> docker compose down
> ```

**To run the published image via Compose instead of building**, either point
`image:` at `ghcr.io/wildcommitter/claudecgwd:latest` and delete the `build:`
block, or just use the `run` command in §4 / the Quadlet unit in §7.

### A note on the persistent-state volume

The mounts bind only your auth, config, project dir, and a **named volume**
`assistant-state` for runtime state (inbox, RAG index, WhatsApp pairing,
reminders, routines, on-demand voices). It's a named volume on purpose: the
image bakes the STT/RAG/TTS venvs and the default voice into that same path, and
a named volume auto-populates from the image on first run — a *bind* mount of an
empty host dir would shadow them and break voice/search.

**After rebuilding the image with updated venvs/models, recreate the volume** to
pick them up:

```sh
podman volume rm assistant-state            # plain `run` / Quadlet
podman volume rm claudecgwd_assistant-state # compose (project-prefixed)
# docker volume rm claudecgwd_assistant-state
```

---

## 6. Optional surfaces

### WhatsApp (second chat surface)

In `config.yaml`:

```yaml
whatsapp:
  enabled: true
  allowed_jids:
    - "34123456789"   # your bare phone number, no '+' and no spaces
```

On first start the pairing **QR code is delivered to you through Telegram** (so
Telegram must be configured too). Scan it from WhatsApp → Settings → Linked
Devices. The pairing persists in the `assistant-state` volume. You drive the bot
from your own "Message Yourself" chat.

### Google Calendar (optional)

1. Create a **Desktop OAuth client** in Google Cloud Console, download its JSON
   to `~/.config/assistant/gcal-oauth.json`.
2. Grant consent once (a browser flow — easiest on the host, or
   `podman exec -it claude-assistant scripts/gcal-auth`):
   ```sh
   cd ~/claudecgwd && scripts/gcal-auth
   ```
   This stores a refresh token at `~/.config/assistant/gcal-token.json`; the
   bridge refreshes it silently thereafter.
3. The relevant env vars (paths, `GCAL_CALENDAR`, defaults to `primary`) are
   documented in `secrets.env`.

Then ask things like "what's on my calendar Friday?" or "add lunch with Ana at
1pm Thursday."

### Voice & RAG on a *native* (non-container) install

These engines are **already baked into the image**, so containerized runs need
nothing extra. If you ever run the bridge natively instead, set the venvs up
once:

```sh
scripts/setup-stt.sh   # faster-whisper (voice-note transcription)
scripts/setup-tts.sh   # piper (spoken replies)
scripts/setup-rag.sh   # fastembed (semantic search)
scripts/setup-gcal.sh  # Google Calendar client
```

---

## 7. Run it as a service (auto-start, auto-restart)

> `Podman` — **recommended: Quadlet** (native Podman + systemd, no compose
> provider, no daemon). Uses [`deploy/assistant.container`](deploy/assistant.container).
> It references the **locally built** image `localhost/claudecgwd-assistant:latest`
> — build it (Option B) or edit the `Image=` line to the ghcr.io ref.
> ```sh
> cd ~/claudecgwd
> podman build -t claudecgwd-assistant:latest .   # if using the local image
> systemctl --user stop assistant 2>/dev/null     # stop any native unit
> systemctl --user disable assistant 2>/dev/null
> mkdir -p ~/.config/containers/systemd
> cp deploy/assistant.container ~/.config/containers/systemd/
> systemctl --user daemon-reload
> systemctl --user start assistant                # unit name = file stem
> journalctl --user -u assistant -f
> ```
> Rebuild after code changes: re-run `podman build`, recreate the state volume
> if venvs changed (§5), then `systemctl --user restart assistant`.
>
> To survive logout/reboot without an active login session:
> `loginctl enable-linger $USER`.

> `Podman` — **alternative: compose-as-a-service.** If you prefer compose,
> [`deploy/assistant-docker.service`](deploy/assistant-docker.service) wraps
> `podman compose up --build`. It needs a compose provider on PATH.
> ```sh
> cp deploy/assistant-docker.service ~/.config/systemd/user/
> systemctl --user daemon-reload
> systemctl --user enable --now assistant-docker
> ```

> `Docker` — the compose file already declares `restart: unless-stopped`, so
> `docker compose up -d` survives daemon restarts. For boot-time start, enable
> the Docker service (`sudo systemctl enable docker`) and keep the container up
> with compose, or wrap `docker compose up` in your own systemd unit modeled on
> `deploy/assistant-docker.service` (swap `podman` → `docker`). The Quadlet unit
> is Podman-only.

---

## 8. Verify

```sh
# Is it up?
podman ps                            # docker ps
# Healthy startup logs (should show the bridges connecting, no auth errors)
podman logs --tail 40 claude-assistant
# claude binary is the pinned version inside the image
podman exec claude-assistant claude --version    # -> 2.1.154 (Claude Code)
```

Then send your bot a message on Telegram. `/health` returns an uptime + state
snapshot; `/help` lists every command.

---

## 9. Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `claude` can't read credentials / permission denied on `~/.claude` | Ownership mismatch. **Podman:** ensure `--userns=keep-id` (or `UserNS=keep-id` in Quadlet). **Docker:** ensure `--user 1000:1000` and that you *are* uid 1000 (`id -u`). |
| Two replies, or messages dropped | A second instance is running on the same token (e.g. the native `assistant` unit). Stop it: `systemctl --user stop assistant`. |
| Voice / `/search` silently fall back to text | The `assistant-state` volume was created from an *old* image, or shadowed by a bind mount. Recreate it (§5) so the baked venvs/models land. |
| `overlay` storage errors on start (Podman, btrfs) | Set the native btrfs driver (§1). |
| `podman compose: command not found` | No compose provider installed. Use the `run` command (§4) or the Quadlet unit (§7) instead. |
| Image bundles drift from the running `claude` | The host's native `claude` may have auto-updated past the pinned version. The image disables auto-update (`DISABLE_AUTOUPDATER=1`); pick one binary to actually run. |
| Calendar says "run gcal-auth" | Expected until you complete the one-time OAuth consent (§6). |
