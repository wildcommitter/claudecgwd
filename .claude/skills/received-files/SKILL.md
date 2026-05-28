---
name: received-files
description: >-
  Catalog and handle files the user sends over a chat surface (Telegram or
  WhatsApp). TRIGGER when an incoming message is a "[file received via ...]"
  notice, or when the user refers to a file they just sent. The bridge has
  already downloaded the file to the inbox; this skill writes it down (records
  it in an index) and helps you work with it.
---

# Received files

When the user sends a file over Telegram or WhatsApp, the bridge downloads it
to the **inbox directory** (default `~/.local/share/assistant/inbox/`, set by
`files.inbox_dir` in config) and then delivers a message to you of the form:

```
[file received via <telegram|whatsapp> — saved to <absolute-path>]
<optional caption / accompanying text>
```

When you see that notice (or the user asks about a file they just sent), do the
following:

## 1. Write it down (catalog it)

Append a row to the index at `<inbox>/INDEX.md` (create the file with a header
if it doesn't exist). One row per received file:

```
| received (UTC)      | source   | filename                  | caption        | path |
|---------------------|----------|---------------------------|----------------|------|
| 2026-05-29 09:14:02 | telegram | invoice.pdf               | "for May"      | /home/user/.local/share/assistant/inbox/20260529-091402-telegram-invoice.pdf |
```

Use the timestamp embedded in the saved filename (`YYYYMMDD-HHMMSS`) for the
"received" column, the `source` from the notice, the original filename (strip
the `YYYYMMDD-HHMMSS-<source>-` prefix), and the caption/accompanying text if
any. Keep the table header at the top; append new rows at the bottom.

## 2. Inspect it if useful

Read the file from its path so you can act on it or summarize it:
- text / code / PDF / CSV / JSON → read directly and summarize or use as asked.
- image → read it (vision) and describe or use it.
- audio/video/other binary → note the type and size; don't try to read bytes.

Only inspect when it helps answer the user; for a bare "here's a file" with no
ask, a short acknowledgement is enough.

## 3. Confirm to the user

Reply on the same surface with a one-line confirmation: what was saved, where,
and (if you inspected it) a short note — e.g. "Saved `invoice.pdf` to the inbox
and logged it. It's a 2-page May invoice totalling €1,240." Don't dump the full
contents unless asked.

## Notes

- The path in the notice is authoritative — always use it; don't guess.
- The inbox and `INDEX.md` persist across restarts. In the container they live
  under the mounted store volume.
- If the notice says the download failed, tell the user it didn't come through
  and ask them to resend.
