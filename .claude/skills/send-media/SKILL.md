---
name: send-media
description: >-
  Send a file — image or document — back to the user over their chat
  surface(s). TRIGGER when the user asks you to send / show / generate a file,
  chart, image, screenshot, PDF, or export, or whenever you produce a file that
  is the actual answer (not just text about it). Replies are text-only by
  default; this is the only way to deliver a file outbound.
---

# Sending files to the user

Your normal reply is text (or a spoken voice note). To deliver an actual
**file** — an image, chart, PDF, CSV, generated document, screenshot — use:

```sh
scripts/send-file <path> [caption]
```

It pushes the file to every configured surface (Telegram + WhatsApp): images go
as a previewable photo, everything else as a document. The caption is optional
but usually worth a short one.

## When to use it

- The user explicitly asks to be **sent/shown** a file ("send me the chart",
  "export that as CSV", "show me the image").
- You **generate a file** that is the deliverable: a plotted chart, a rendered
  diagram, a report document, a screenshot, a converted/cropped image.
- You fetched or produced a binary the user wants to keep.

Do **not** use it for plain prose answers — just reply normally. Don't send a
file the user didn't ask for and that text already covers.

## How

1. Create the file somewhere readable (e.g. under the inbox dir or /tmp). Use
   the right tool — write a chart with a quick Python/matplotlib script, render
   markdown to PDF, screenshot, etc.
2. Run `scripts/send-file /abs/path "short caption"`.
3. In your text reply, briefly note what you sent ("Sent the revenue chart as a
   PNG."). The file arrives as a separate message.

## Notes

- Delivery rides the same proactive path as `scripts/notify.sh`, so it reaches
  the user even though it isn't part of the text reply.
- Big files: Telegram caps bot uploads at 50 MB; keep things reasonable.
- Clean up scratch files in /tmp afterward; files you want catalogued/searchable
  belong in the inbox dir.
