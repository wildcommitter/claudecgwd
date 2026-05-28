# AGENTS.md

Guidance for AI coding agents working in this repository.

## What this is

`claudecgwd` is a personal-assistant bridge: a Go service that spawns the
interactive `claude` (Claude Code) CLI under a PTY, reads its session
transcript, and bridges I/O to **Telegram** and **WhatsApp** for a single user.
One warm Claude session is shared across both surfaces.

It is single-user by design. Do **not** add multi-user features, a web UI, or
progressive streaming edits unless explicitly asked.

## Build / test / run

- **Go toolchain lives at `~/.local/go/bin/go`** (not on the default PATH, not
  via pacman). Use that path.
- **Canonical build:** `./scripts/install.sh` — runs `go test ./...`, builds
  `~/.local/bin/assistant`, and restarts the `assistant` systemd --user unit.
  Use this instead of bare `go build`.
  - `./scripts/install.sh --no-restart` builds + installs without restarting.
- **Tests:** `~/.local/go/bin/go test ./...`. Always `gofmt -w` changed files.
- A **post-commit git hook** runs `install.sh` automatically. To skip it on a
  commit (e.g. docs-only, or to avoid a restart): `ASSISTANT_SKIP_INSTALL=1 git
  commit ...`.

## Layout

```
cmd/assistant   main wiring (config -> driver + bridges + router + notifier)
cmd/smoke       one-shot driver test (no bots)
cmd/ptydump     PTY/TUI debugging helper
internal/claude driver.go (PTY + vt10x), transcript.go (read replies from
                ~/.claude/projects/<slug>/<session>.jsonl), interactive.go
                (AskUserQuestion handling)
internal/bridge router.go, telegram.go, whatsapp.go, notify.go, files.go,
                stt.go, types.go (Origin/Bridge/Inbound interfaces)
internal/config config.go (single Config struct, yaml)
scripts/        install.sh, watch-ci.sh, notify.sh, transcribe.py
deploy/         systemd units, Quadlet, secrets.env.example
docs/DOCKER.md  sandboxed Podman/Docker deployment
.claude/skills/ project skills (e.g. received-files)
```

## Architecture & invariants

- **One claude session**, pinned via `--session-id` and resumed with
  `--resume`, run with `--permission-mode bypassPermissions`. All inbound
  messages (every surface) feed one FIFO `inbound` channel; the router
  processes them one at a time.
- **Reply extraction reads the transcript, not the screen.** `driver.Send`
  waits for a terminal `stop_reason` in the session JSONL and returns the
  assistant text from there. Screen-scraping (`extractResponse`) is a fallback
  only — don't rely on it.
- **Surfaces implement `bridge.Origin`** (Describe / NotifyPending / Reply /
  AskChoices) and run as a `bridge.Bridge`. To add a surface, implement those
  and wire it in `cmd/assistant`.
- **WhatsApp** uses whatsmeow (linked-device). The operator drives it from the
  "Message Yourself" chat (detected by `chat == sender`); replies go to the
  phone-number self-JID; the bot tracks its own sent IDs to avoid reply loops.
  CGO-free SQLite (`modernc.org/sqlite`) is aliased to the `sqlite3` dialect.
- **Files** sent over chat are downloaded to `files.inbox_dir`
  (`~/.local/share/assistant/inbox`); the session gets a `[file received ...]`
  notice and the `received-files` skill catalogs them.
- **Voice/audio** is transcribed locally (faster-whisper venv via
  `scripts/transcribe.py`) and fed in as the prompt text.
- **Proactive notifications:** a reply only reaches the user on an inbound
  turn. To push unprompted (e.g. a finished background job), write to the
  notify FIFO via `scripts/notify.sh "msg"` — the Notifier fans it out to all
  surfaces.

## Conventions

- Match the surrounding code style; keep comments at the existing density and
  explain *why*, not *what*.
- Commit messages: imperative subject, a short body explaining the why, ending
  with `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`.
- Commit/push only when asked. Verify (build + test, and live where possible)
  before committing.

## Gotchas (learned the hard way)

- **Committing restarts the running bot** (post-commit hook). If you're the
  assistant running *inside* this very session, a synchronous restart kills you
  mid-turn — commit with `ASSISTANT_SKIP_INSTALL=1`, then deploy via a
  **detached** restart so your reply is delivered first:
  `systemd-run --user --on-active=30 systemctl --user restart assistant`.
- **CI:** the repo is public; `scripts/watch-ci.sh [SHA]` polls the GitHub
  Actions run to completion (no gh/token). The publish workflow pushes the
  image to `ghcr.io/wildcommitter/claudecgwd`.
- **Podman on btrfs** needs the native btrfs storage driver
  (`~/.config/containers/storage.conf` → `driver = "btrfs"`); the default
  overlay driver fails.
- Verify external-library APIs against the module cache
  (`$(go env GOMODCACHE)`) before coding — they're version-sensitive.
