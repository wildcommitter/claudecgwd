---
name: calendar
description: >-
  Read and create events on the user's Google Calendar. TRIGGER when the user
  asks about their schedule / agenda / what's coming up, or asks to add, create,
  schedule, or book an event or appointment ("what's on tomorrow?", "am I free
  Friday afternoon?", "add lunch with Ana at 1pm Thursday"). Distinct from
  reminders (which just ping) and routines (recurring prompts).
---

# Google Calendar

The bridge talks to Google Calendar via `scripts/gcal` (service-account auth).
Use it whenever the user asks about or wants to change their calendar.

```sh
gcal agenda [--days N]                 # upcoming events (default 7 days)
gcal add --start <ISO> [--end <ISO>] --title <T> [--location L] [--description D]
gcal find --query <text> [--days N]    # search upcoming events
```

## Reading

- "what's on today / this week / am I free Friday" → `gcal agenda --days N`
  (pick N to cover the window) and summarize naturally. For a specific
  search, use `gcal find --query "dentist"`.
- Times come back in RFC3339; present them in plain language ("Thursday at 1pm").

## Creating

- Convert the user's natural time into explicit ISO local datetimes — **you** do
  the date math (you know today's date), then pass `--start`/`--end`. A naive
  ISO like `2026-05-30T13:00:00` is interpreted in the host's local timezone.
- Default duration is 1 hour if no end given. Include `--location`/`--description`
  when the user mentions them.
- **Confirm before creating** if the time is ambiguous ("next Friday" near a
  weekend, no year, etc.); otherwise just create it and report back with the
  resolved time.

## Notes

- Auth is OAuth as the user: a one-time `scripts/gcal-auth` (browser consent)
  stores a refresh token; `gcal` refreshes silently after. `$GCAL_CALENDAR`
  defaults to `primary` (the user's own calendar) — no sharing needed.
- If `gcal` says "not authorized yet — run scripts/gcal-auth", the calendar
  isn't set up; tell the user to run that once (it's interactive, opens a
  browser) — don't guess event data.
- A morning-agenda routine pairs well with this: `scripts/routine add "daily 08:00"
  "Post my calendar agenda for today"`.
