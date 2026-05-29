# claudecgwd

A personal-assistant bridge: drives an interactive `claude` (Claude Code) TUI
under a PTY and routes its I/O to **Telegram and WhatsApp** for a single user.

One warm Claude session, reachable from chat, with context preserved across
restarts — plus voice notes, image understanding, file handling, reminders,
and local semantic search over everything you've sent and said.

## Architecture

```
 Telegram ─┐
           ├─► inbound queue ─► Claude driver (PTY + vt10x) ─► reply ─► source
 WhatsApp ─┘                            │
                                 reads the reply from the
                                 session transcript (JSONL)
```

- **Telegram:** [`github.com/go-telegram/bot`](https://github.com/go-telegram/bot)
- **WhatsApp:** [`go.mau.fi/whatsmeow`](https://go.mau.fi/whatsmeow) (linked device)
- **PTY:** [`github.com/creack/pty`](https://github.com/creack/pty)
- **Virtual terminal:** [`github.com/hinshun/vt10x`](https://github.com/hinshun/vt10x)

The bridge spawns `claude` with `--session-id <fixed-uuid>` so the same
conversation survives restarts (resumed via `--resume`). Every surface feeds a
single FIFO queue and one message is processed at a time. Replies are read from
Claude Code's authoritative session transcript (JSONL), not screen-scraped —
the TUI parse is only a fallback.

## Features

- **Two chat surfaces** — Telegram and WhatsApp (linked device; the pairing QR
  is delivered through Telegram). All surfaces share the one Claude session.
- **Voice notes, both ways** — incoming notes are transcribed locally
  (faster-whisper); replies can be spoken back as a voice note (piper TTS),
  mirroring the modality you used.
- **Multilingual audio** — `/speech <language|country>` sets the audio
  language for both engines. Whisper is multilingual out of the box; piper
  voices (≈25 languages) download on demand the first time you switch.
- **Image understanding** — a sent photo becomes a vision turn (Claude opens it
  with its Read tool), not just a saved file.
- **File handling** — any sent file is saved to an inbox and catalogued.
- **Reminders** — "remind me at 6pm to…" fires a proactive ping when it's due.
- **Semantic search (RAG)** — local embeddings over your attachments + past
  conversations; ask to recall something, or use `/search`.
- **Proactive notifications** — background jobs can push to every surface.
- **Resilient** — outbound sends retry with backoff and the bridges reconnect
  on a network drop instead of taking the process down.

### Chat commands

| Command | Effect |
|---------|--------|
| `/new` | Start a fresh conversation (clears context) |
| `/project <name\|dir>` | Switch project; a bare name is wildcard-matched against tracked projects |
| `/projects` | List tracked project directories |
| `/search <query>` | Semantic search over attachments + past conversations |
| `/voice <on\|off\|auto>` | Spoken replies: always / never / mirror voice notes |
| `/speech <language\|country>` | Set the audio language (transcription + voice); voices download on demand |
| `/status` | Show the current project and session |
| `/health` | Uptime + a snapshot of the bridge's state |
| `/help` | List these commands |

Unknown slash text is passed through to Claude untouched.

## Install

```sh
# Build, install ~/.local/bin/assistant, restart systemd unit (if enabled)
./scripts/install.sh

# Config
mkdir -p ~/.config/assistant
cp config.example.yaml ~/.config/assistant/config.yaml
cp deploy/secrets.env.example ~/.config/assistant/secrets.env
chmod 600 ~/.config/assistant/secrets.env

# Generate a stable session ID
uuidgen   # paste into config.yaml under claude.session_id

# Edit ~/.config/assistant/config.yaml and ~/.config/assistant/secrets.env
```

### Telegram setup

1. Message [@BotFather](https://t.me/BotFather), `/newbot`, save the token to `~/.config/assistant/secrets.env` as `TELEGRAM_BOT_TOKEN=...`.
2. Message [@userinfobot](https://t.me/userinfobot) to get your numeric user ID, paste it under `telegram.allowed_user_ids`.
3. Start a chat with your new bot.

### WhatsApp setup (optional)

Set `whatsapp.enabled: true` and list your bare phone number under
`whatsapp.allowed_jids`. On first start the pairing QR is sent to you **through
Telegram** (so Telegram must be configured too); scan it from WhatsApp →
Settings → Linked Devices. The session is stored and resumed on later runs. You
drive the bot from your own "Message Yourself" chat.

### Voice + RAG search (optional, local)

Both run on local CPU models — no API key, nothing leaves the box. They use
their own Python venvs; set them up once (the Docker image bakes them in):

```sh
scripts/setup-stt.sh    # faster-whisper for voice-note transcription
scripts/setup-rag.sh    # fastembed embeddings for /search + recall
scripts/setup-tts.sh    # piper for spoken (voice-note) replies
```

Then set `stt.enabled: true` / `tts.enabled: true` in config. Build/refresh the
search index with `scripts/rag index` (incremental); query with
`scripts/rag query "<text>"`.

### systemd

```sh
mkdir -p ~/.config/systemd/user
cp deploy/assistant.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now assistant
journalctl --user -u assistant -f
```

## Run with Docker / Podman

A sandboxed container image is provided. This is the short version; see
[docs/DOCKER.md](docs/DOCKER.md) for the full guide (sandbox boundary table,
rootless Podman notes, and the Quadlet unit).

> **Stop the native service first** — otherwise two instances fight over the
> Telegram token and the session transcript:
>
> ```sh
> systemctl --user stop assistant
> ```

```sh
cd ~/claudecgwd

# Build the image and run (foreground, watch logs)
docker compose up --build

# Or detached
docker compose up -d --build

# Follow logs / stop
docker compose logs -f
docker compose down
```

The image bakes in a pinned `claude` and runs as your uid; the compose file
bind-mounts only `~/.claude`, `~/.claude.json`, `~/.config/assistant` (ro), and
the project dir — nothing else of the host is visible to the sandboxed session.

**Podman:** `alias docker=podman` makes the commands above work. The compose file
sets `userns: keep-id` for rootless bind-mount ownership. For a daemon-free
systemd deployment, use the Quadlet unit `deploy/assistant.container` instead
(see docs/DOCKER.md). On a btrfs host, Podman needs its native btrfs storage
driver (`~/.config/containers/storage.conf` → `driver = "btrfs"`).

## Development loop

Use `./scripts/install.sh` instead of bare `go build` — it runs tests, installs
the binary to `~/.local/bin/assistant`, and restarts the systemd unit so the
running bot picks up your changes. The script also (re)installs a
`post-commit` git hook from `scripts/post-commit.hook` that does the same
thing automatically after every commit. To skip on a particular commit:

```sh
ASSISTANT_SKIP_INSTALL=1 git commit ...
```

## Known limitations & notes

- **Single user.** Built for one operator; there is no multi-user/web UI and
  the allowlists are deliberately narrow.
- **One conversation at a time.** All surfaces share a single Claude session.
  `/new` clears it and `/project` switches the working directory (each a fresh
  conversation), but there's no concurrent multi-session support.
- **No streaming.** A reply is sent once the turn reaches a terminal stop; long
  turns post a "working…" heartbeat so you know it isn't stuck.
- **Reply capture.** Read from the session transcript (reliable); the TUI
  screen-scrape in `internal/claude/driver.go` is only a fallback for when the
  transcript can't be located.
- **At-least-once sends.** The retry policy can rarely deliver a duplicate if a
  request reached the server but its response was lost — an accepted trade for
  not dropping messages during a network blip.
- **No tool-use approval UI.** Runs with `bypassPermissions` — Claude can
  read/edit files and run shell as you. The Docker sandbox bounds the blast
  radius; trust the binary.
