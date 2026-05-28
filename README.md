# claudecgwd

A personal-assistant bridge: drives an interactive `claude` (Claude Code) TUI under
a PTY and routes its I/O to Telegram for a single user.

One warm Claude session, reachable from chat, with context preserved across
restarts. (WhatsApp is the next planned surface.)

## Architecture

```
 Telegram ──► inbound queue ──► Claude driver (PTY + vt10x) ──► reply
                                                        │
                                               back to the source
```

- **Telegram:** [`github.com/go-telegram/bot`](https://github.com/go-telegram/bot)
- **PTY:** [`github.com/creack/pty`](https://github.com/creack/pty)
- **Virtual terminal:** [`github.com/hinshun/vt10x`](https://github.com/hinshun/vt10x)
- **ANSI strip:** [`github.com/charmbracelet/x/ansi`](https://github.com/charmbracelet/x/ansi)

The bridge spawns `claude` with `--session-id <fixed-uuid>` so the same
conversation survives restarts (resumed via `--resume`). Only one message is
processed at a time, feeding a single FIFO queue.

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

### systemd

```sh
mkdir -p ~/.config/systemd/user
cp deploy/assistant.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now assistant
journalctl --user -u assistant -f
```

## Development loop

Use `./scripts/install.sh` instead of bare `go build` — it runs tests, installs
the binary to `~/.local/bin/assistant`, and restarts the systemd unit so the
running bot picks up your changes. The script also (re)installs a
`post-commit` git hook from `scripts/post-commit.hook` that does the same
thing automatically after every commit. To skip on a particular commit:

```sh
ASSISTANT_SKIP_INSTALL=1 git commit ...
```

## Known limitations (v1)

- **Response capture is heuristic.** The TUI redraws aggressively; the driver
  ANSI-strips the raw byte stream and filters status/prompt/separator lines.
  For long or unusual outputs, expect occasional garbage or truncation. Tune
  `extractResponse` in `internal/claude/driver.go` if you hit problems.
- **No streaming.** Replies are sent once Claude returns to its ready prompt.
- **Single session.** All messages share one Claude conversation.
- **No tool-use approval UI.** Runs with `bypassPermissions` — Claude can
  read/edit files and run shell as you. Trust the binary.
