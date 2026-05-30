#!/usr/bin/env python3
"""Google Calendar OAuth bootstrap — chat-driven and headless.

The bridge runs on a server with no browser; the user authorizes from a chat
surface (Telegram / WhatsApp). So authorization is a two-step, copy-paste flow
that Claude can drive conversationally:

    gcal-auth url                 # print the consent URL to send to the user
    gcal-auth exchange <code|url> # exchange the pasted code / redirect URL

Step 1 prints a Google consent URL. The user opens it on their own device,
grants access, and is redirected to a (dead) http://localhost page — they copy
that address-bar URL, or just the `code=...` value, and send it back. Step 2
turns it into a token saved to $GCAL_TOKEN; thereafter gcal refreshes silently.

`gcal-auth browser` keeps the old local-browser flow for a machine with a
display. With no arguments, defaults to `url`.

PKCE: the code_verifier generated in step 1 must reach step 2, which runs in a
separate process, so it is stashed next to the token between the steps and
removed on success.
"""
import json
import os
import sys
from urllib.parse import parse_qs, urlparse

CLIENT = os.environ.get("GCAL_OAUTH_CLIENT", os.path.expanduser("~/.config/assistant/gcal-oauth.json"))
TOKEN = os.environ.get("GCAL_TOKEN", os.path.expanduser("~/.config/assistant/gcal-token.json"))
STATE = os.environ.get("GCAL_AUTH_STATE", os.path.expanduser("~/.config/assistant/gcal-auth-state.json"))
SCOPES = ["https://www.googleapis.com/auth/calendar"]
# Loopback redirect: the consent page redirects here; nothing needs to listen
# because the user simply copies the resulting URL back to us.
REDIRECT_URI = "http://localhost"


def _require_client():
    if not os.path.exists(CLIENT):
        print(f"OAuth client JSON not found at {CLIENT}.\n"
              f"Download it from Google Cloud Console -> Credentials -> your OAuth "
              f"client (type: Desktop app) -> Download JSON, and save it there "
              f"(or set GCAL_OAUTH_CLIENT).", file=sys.stderr)
        sys.exit(1)


def _already_valid():
    from google.oauth2.credentials import Credentials

    if not os.path.exists(TOKEN):
        return False
    creds = Credentials.from_authorized_user_file(TOKEN, SCOPES)
    return bool(creds and creds.valid)


def cmd_url():
    from google_auth_oauthlib.flow import Flow

    _require_client()
    if _already_valid():
        print("Already authorized — nothing to do.")
        return 0

    flow = Flow.from_client_secrets_file(CLIENT, scopes=SCOPES, redirect_uri=REDIRECT_URI)
    auth_url, _ = flow.authorization_url(
        access_type="offline",
        include_granted_scopes="true",
        prompt="consent",
    )
    # Persist the PKCE verifier so the separate `exchange` process can finish the
    # handshake. code_verifier may be None on libs without PKCE — that's fine.
    os.makedirs(os.path.dirname(STATE), exist_ok=True)
    with open(STATE, "w") as f:
        json.dump({"code_verifier": flow.code_verifier}, f)
    os.chmod(STATE, 0o600)

    print(auth_url)
    return 0


def _extract_code(arg):
    arg = arg.strip()
    if arg.startswith("http://") or arg.startswith("https://"):
        code = parse_qs(urlparse(arg).query).get("code", [None])[0]
        if not code:
            print("No 'code' parameter found in that URL.", file=sys.stderr)
            sys.exit(1)
        return code
    return arg


def cmd_exchange(arg):
    from google_auth_oauthlib.flow import Flow

    _require_client()
    code = _extract_code(arg)

    code_verifier = None
    if os.path.exists(STATE):
        with open(STATE) as f:
            code_verifier = json.load(f).get("code_verifier")

    flow = Flow.from_client_secrets_file(
        CLIENT, scopes=SCOPES, redirect_uri=REDIRECT_URI, code_verifier=code_verifier
    )
    flow.fetch_token(code=code)
    creds = flow.credentials
    os.makedirs(os.path.dirname(TOKEN), exist_ok=True)
    with open(TOKEN, "w") as f:
        f.write(creds.to_json())
    os.chmod(TOKEN, 0o600)
    if os.path.exists(STATE):
        os.remove(STATE)
    print(f"authorized — token saved to {TOKEN}")
    return 0


def cmd_browser():
    """Legacy local-browser flow — only for a machine with a display."""
    from google_auth_oauthlib.flow import InstalledAppFlow

    _require_client()
    if _already_valid():
        print("Already authorized — nothing to do.")
        return 0
    flow = InstalledAppFlow.from_client_secrets_file(CLIENT, SCOPES)
    creds = flow.run_local_server(
        port=0, open_browser=True,
        authorization_prompt_message="Open this URL to authorize calendar access:\n{url}",
        success_message="Calendar access granted — you can close this tab.",
    )
    os.makedirs(os.path.dirname(TOKEN), exist_ok=True)
    with open(TOKEN, "w") as f:
        f.write(creds.to_json())
    os.chmod(TOKEN, 0o600)
    print(f"authorized — token saved to {TOKEN}")
    return 0


def main():
    cmd = sys.argv[1] if len(sys.argv) > 1 else "url"
    if cmd == "url":
        return cmd_url()
    if cmd == "exchange":
        if len(sys.argv) < 3:
            print("Usage: gcal-auth exchange <code-or-redirect-url>", file=sys.stderr)
            return 1
        return cmd_exchange(sys.argv[2])
    if cmd == "browser":
        return cmd_browser()
    if cmd in ("-h", "--help", "help"):
        print(__doc__)
        return 0
    print(f"Unknown command: {cmd}\n", file=sys.stderr)
    print(__doc__, file=sys.stderr)
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
