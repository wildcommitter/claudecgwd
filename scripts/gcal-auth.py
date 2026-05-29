#!/usr/bin/env python3
"""One-time Google Calendar OAuth consent. Opens a browser, you approve, and the
refresh token is saved for the bridge to use thereafter.

Reads the OAuth client JSON (Desktop-app type) at $GCAL_OAUTH_CLIENT and writes
the token to $GCAL_TOKEN. Run interactively on a machine with a browser:

    scripts/gcal-auth
"""
import os
import sys

CLIENT = os.environ.get("GCAL_OAUTH_CLIENT", os.path.expanduser("~/.config/assistant/gcal-oauth.json"))
TOKEN = os.environ.get("GCAL_TOKEN", os.path.expanduser("~/.config/assistant/gcal-token.json"))
SCOPES = ["https://www.googleapis.com/auth/calendar"]


def main() -> int:
    if not os.path.exists(CLIENT):
        print(f"OAuth client JSON not found at {CLIENT}.\n"
              f"Download it from Google Cloud Console → Credentials → your OAuth "
              f"client (type: Desktop app) → Download JSON, and save it there "
              f"(or set GCAL_OAUTH_CLIENT).", file=sys.stderr)
        return 1
    from google_auth_oauthlib.flow import InstalledAppFlow

    flow = InstalledAppFlow.from_client_secrets_file(CLIENT, SCOPES)
    # Loopback flow: opens the browser and captures the redirect on localhost.
    # Falls back to printing a URL if no browser can be opened.
    creds = flow.run_local_server(port=0, open_browser=True,
                                  authorization_prompt_message="Open this URL to authorize calendar access:\n{url}",
                                  success_message="Calendar access granted — you can close this tab.")
    os.makedirs(os.path.dirname(TOKEN), exist_ok=True)
    with open(TOKEN, "w") as f:
        f.write(creds.to_json())
    os.chmod(TOKEN, 0o600)
    print(f"authorized — token saved to {TOKEN}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
