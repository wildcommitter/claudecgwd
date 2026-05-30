# gcal — Google Calendar helper

Thin CLI over the Google Calendar API, plus a chat-driven OAuth bootstrap.

## Why the auth flow is two steps

The bridge runs headless on a server. The old flow used
`InstalledAppFlow.run_local_server()`, which opens a browser **on that
server** — useless when the user is on Telegram or WhatsApp.

`gcal-auth` instead exposes authorization as a copy-paste flow that Claude
drives over whichever messenger the user is on:

1. `gcal-auth url` prints a Google consent URL.
2. Claude relays the URL to the user. They open it on their own device and
   grant access.
3. Google redirects to `http://localhost/?code=...`. Nothing is listening, so
   the browser shows a connection error — but the address bar holds the code.
   The user copies that whole URL (or just the `code=` value) back into chat.
4. Claude runs `gcal-auth exchange <code-or-url>`, which mints and saves the
   token. Thereafter the token refreshes itself.

The PKCE `code_verifier` from step 1 is stashed in `gcal-auth-state.json` so the
separate `exchange` process can finish the handshake; it is deleted on success.

`gcal-auth browser` keeps the old local-browser flow for a desktop with a
display.

The bridge also exposes this as a native chat command on **both Telegram and
WhatsApp**: `/calauth` runs step 1 and relays the link; `/calauth <pasted-url>`
runs step 2. (See `internal/bridge/router.go`, `handleCalAuth`.)

## Files

- `setup-gcal.sh` — create the venv, install deps, print next steps.
- `gcal-auth` / `gcal-auth.py` — the OAuth bootstrap (`url` / `exchange` / `browser`).
- `gcal` / `gcal.py` — agenda / add / find.

Config lives in `~/.config/assistant/` (override via env):

- `GCAL_OAUTH_CLIENT` — OAuth client JSON, default `gcal-oauth.json`
- `GCAL_TOKEN` — saved user token, default `gcal-token.json`
- `GCAL_CALENDAR` — calendar id, default `primary`
