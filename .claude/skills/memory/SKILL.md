---
name: memory
description: >-
  Remember durable facts about the user across sessions. TRIGGER when the user
  tells you to remember something, states a lasting preference or personal
  detail ("I prefer metric", "my wife is Ana", "always answer in Spanish", "my
  work hours are 9–5"), or asks what you remember / to forget something. This is
  curated long-term memory, separate from RAG search over past transcripts.
---

# Persistent memory

The bridge keeps a small set of durable facts about the user and injects them
into the first prompt of every session, so they carry across `/new`, `/project`,
and restarts. This is for **stable facts**, not transient conversation detail.

## Remembering

When the user shares something worth keeping long-term, store it:

```sh
scripts/remember "<concise fact>"
```

Good things to remember: preferences (units, language, tone, formats), people
and relationships, recurring projects, locations, working hours, standing
instructions. Write each as a short standalone fact ("Prefers answers in metric
units."), not a transcript of the conversation.

Be judicious:
- Only store things with **lasting** value. Don't remember one-off task details,
  things already obvious from context, or anything sensitive the user wouldn't
  want persisted (card numbers, passwords).
- If the user *explicitly* says "remember …", always store it.
- `scripts/remember --list` shows the current set.

## Recalling

The stored facts are already injected into your context at the start of each
session (you'll see a "[persistent memory …]" block), so just use them
naturally — don't recite them unprompted. For questions about *past
conversations or sent files*, that's the `rag-search` skill, not this.

## User-facing management

The user manages memory from chat (these are bridge commands, not yours to run):
- `/memory` — list what's remembered.
- `/forget <text>` — drop facts matching text; `/forget all` clears everything.

## Notes

- Store: `~/.local/share/assistant/memory.md` (markdown bullets), or
  `$CLAUDECGWD_MEMORY`.
- New facts added mid-session take effect for the rest of *this* session (you
  already know them from the conversation) and are injected automatically in
  future sessions.
