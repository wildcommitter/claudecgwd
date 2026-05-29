---
name: rag-search
description: >-
  Semantic (RAG) search over the user's received attachments and past
  conversations. TRIGGER when the user asks you to find, recall, or look up
  something from earlier chats or sent files ("what did we decide about X",
  "find that invoice", "what was the address she sent", "have we discussed Y"),
  or after a new attachment arrives (to keep the index fresh). Local embeddings
  — nothing leaves the box.
---

# RAG search

A local index over two corpora lets you answer questions grounded in history:

- **Attachments** — every file received over chat (inbox: text, PDFs, docx,
  code, CSV…). Audio content isn't indexed as a file because its transcript is
  already in the conversation corpus.
- **Conversations** — all Claude session transcripts (user + assistant turns)
  across projects.

Embeddings are local (fastembed/ONNX, model `BAAI/bge-small-en-v1.5`); the index
is a SQLite file at `~/.local/share/assistant/rag/index.db`. The single
entrypoint is the `scripts/rag` wrapper.

```sh
rag index [--scope both|attachments|conversations] [--project-only]
rag query "<text>" [-k N] [--json]
rag stats
```

## Retrieving (the RAG part)

When the user asks you to find / recall / look something up:

1. Run `scripts/rag query "<their question, rephrased as a search query>" --json`
   (use `--json` so you can parse scores and sources cleanly; `-k 6` is a good
   default, raise it for broad questions).
2. Read the returned chunks. Each has a `kind` (`attachment`/`conversation`), a
   `ref` (filename, or session id), and the matching `text`.
3. **Synthesize** an answer from the retrieved text — don't just dump chunks.
   Cite where it came from in plain language ("from the invoice you sent on…",
   "earlier we agreed…"). If a chunk is from an attachment and you need detail,
   open the file by name from the inbox and read it.
4. If nothing relevant comes back (low scores / empty), say so honestly rather
   than guessing — the index may simply not contain it yet.

`/search <query>` is the user-facing raw view of the same index (ranked
snippets, no synthesis); you don't need to invoke it — it's the bridge command.

## Keeping the index fresh

- The index is incremental: indexed inbox files are skipped; transcripts are
  read forward from a saved cursor, so only new turns get embedded.
- **After a new attachment arrives** (a `[file received …]` / `[image received …]`
  notice), run `scripts/rag index --scope attachments` so it's searchable.
- To pick up recent conversation turns before a recall query, a quick
  `scripts/rag index` first keeps results current.
- A full first index can take a while (it embeds everything). Run long indexes
  in the background and, when done, notify via `scripts/notify.sh` rather than
  assuming a completion message will reach the user.

## Notes

- Local only — no API key, no data leaves the machine. In the container the
  index and venv live under the mounted store volume, so they persist.
- Retrieval is brute-force cosine; fine to tens of thousands of chunks. If it
  ever grows beyond that, revisit with an ANN index.
- The query is matched as a passage-retrieval search; phrase it as what you're
  looking for, not as a yes/no question.
