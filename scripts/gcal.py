#!/usr/bin/env python3
"""Google Calendar helper (OAuth user auth).

Subcommands:
  agenda [--days N]                      list upcoming events (default 7 days)
  add --start ISO [--end ISO] --title T [--location L] [--description D]
                                         create an event (naive ISO = local tz)
  find --query Q [--days N]              search upcoming events by text

Auth: OAuth as the user. scripts/gcal-auth runs the one-time browser consent and
stores a refresh token at $GCAL_TOKEN (default ~/.config/assistant/gcal-token.json)
from the OAuth client JSON at $GCAL_OAUTH_CLIENT (default
~/.config/assistant/gcal-oauth.json). Access tokens refresh silently here.
$GCAL_CALENDAR is the calendar id ("primary" = your own main calendar).

Run via the gcal venv's interpreter (see the scripts/gcal wrapper).
"""
import argparse
import datetime
import os
import sys

TOKEN = os.environ.get("GCAL_TOKEN", os.path.expanduser("~/.config/assistant/gcal-token.json"))
CAL = os.environ.get("GCAL_CALENDAR", "primary")
SCOPES = ["https://www.googleapis.com/auth/calendar"]


def service():
    from google.oauth2.credentials import Credentials
    from google.auth.transport.requests import Request
    from googleapiclient.discovery import build

    if not os.path.exists(TOKEN):
        print("google calendar not authorized yet — run the chat-driven auth: "
              "`scripts/gcal-auth url`, send the user the URL, then "
              "`scripts/gcal-auth exchange <code>` with what they paste back.",
              file=sys.stderr)
        sys.exit(1)
    creds = Credentials.from_authorized_user_file(TOKEN, SCOPES)
    if not creds.valid:
        if creds.expired and creds.refresh_token:
            creds.refresh(Request())
            _save_token(creds)
        else:
            print("google calendar authorization is invalid/expired — re-run the "
                  "chat-driven auth: `scripts/gcal-auth url` then "
                  "`scripts/gcal-auth exchange <code>`.", file=sys.stderr)
            sys.exit(1)
    return build("calendar", "v3", credentials=creds, cache_discovery=False)


def _save_token(creds):
    with open(TOKEN, "w") as f:
        f.write(creds.to_json())
    os.chmod(TOKEN, 0o600)


def local_tz():
    return datetime.datetime.now().astimezone().tzinfo


def parse_when(s):
    dt = datetime.datetime.fromisoformat(s)
    if dt.tzinfo is None:  # naive → assume the host's local timezone
        dt = dt.replace(tzinfo=local_tz())
    return dt


def fmt_event(e):
    start = e["start"].get("dateTime", e["start"].get("date", "?"))
    summary = e.get("summary", "(no title)")
    loc = e.get("location", "")
    line = f"- {start}  {summary}"
    if loc:
        line += f"  @ {loc}"
    return line


def cmd_agenda(args):
    svc = service()
    now = datetime.datetime.now(local_tz())
    end = now + datetime.timedelta(days=args.days)
    items = svc.events().list(
        calendarId=CAL, timeMin=now.isoformat(), timeMax=end.isoformat(),
        singleEvents=True, orderBy="startTime", maxResults=50,
    ).execute().get("items", [])
    if not items:
        print(f"No events in the next {args.days} day(s).")
        return 0
    for e in items:
        print(fmt_event(e))
    return 0


def cmd_add(args):
    svc = service()
    start = parse_when(args.start)
    end = parse_when(args.end) if args.end else start + datetime.timedelta(hours=1)
    body = {
        "summary": args.title,
        "start": {"dateTime": start.isoformat()},
        "end": {"dateTime": end.isoformat()},
    }
    if args.location:
        body["location"] = args.location
    if args.description:
        body["description"] = args.description
    ev = svc.events().insert(calendarId=CAL, body=body).execute()
    print(f"created: {ev.get('summary')} — {start.isoformat()} → {end.isoformat()}")
    if ev.get("htmlLink"):
        print(ev["htmlLink"])
    return 0


def cmd_find(args):
    svc = service()
    now = datetime.datetime.now(local_tz())
    end = now + datetime.timedelta(days=args.days)
    items = svc.events().list(
        calendarId=CAL, q=args.query, timeMin=now.isoformat(), timeMax=end.isoformat(),
        singleEvents=True, orderBy="startTime", maxResults=25,
    ).execute().get("items", [])
    if not items:
        print(f"No events matching {args.query!r} in the next {args.days} day(s).")
        return 0
    for e in items:
        print(fmt_event(e))
    return 0


def main():
    ap = argparse.ArgumentParser(description="Google Calendar helper")
    sub = ap.add_subparsers(dest="cmd", required=True)

    pa = sub.add_parser("agenda")
    pa.add_argument("--days", type=int, default=7)
    pa.set_defaults(func=cmd_agenda)

    pad = sub.add_parser("add")
    pad.add_argument("--start", required=True)
    pad.add_argument("--end")
    pad.add_argument("--title", required=True)
    pad.add_argument("--location")
    pad.add_argument("--description")
    pad.set_defaults(func=cmd_add)

    pf = sub.add_parser("find")
    pf.add_argument("--query", required=True)
    pf.add_argument("--days", type=int, default=30)
    pf.set_defaults(func=cmd_find)

    args = ap.parse_args()
    return args.func(args)


if __name__ == "__main__":
    raise SystemExit(main())
