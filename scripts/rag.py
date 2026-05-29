#!/usr/bin/env python3
"""Local RAG index over the assistant's attachments and conversations.

Subcommands:
  index            (re)build the index incrementally from the inbox + transcripts
  query <text>     retrieve the top-k most relevant chunks (prints text + source)
  stats            show what's indexed

Design notes:
- Embeddings are LOCAL (fastembed / ONNX, CPU) — nothing leaves the box, in
  keeping with the rest of the bridge. Default model: BAAI/bge-small-en-v1.5.
- Storage is a single SQLite file (Python stdlib); embeddings are stored as
  float32 blobs. Retrieval is brute-force cosine over all vectors, which is
  more than fast enough at personal scale (tens of thousands of chunks).
- Indexing is incremental: immutable inbox files are skipped once indexed;
  append-only session transcripts are read forward from a saved byte cursor, so
  only new turns get embedded.

Run with the RAG venv's interpreter (see scripts/rag wrapper).
"""
import argparse
import glob
import hashlib
import json
import os
import sqlite3
import struct
import sys
from pathlib import Path

HOME = Path(os.path.expanduser("~"))
DEFAULT_INDEX = HOME / ".local/share/assistant/rag/index.db"
DEFAULT_INBOX = os.environ.get("ASSISTANT_INBOX", str(HOME / ".local/share/assistant/inbox"))
DEFAULT_PROJECTS = HOME / ".claude/projects"
DEFAULT_MODEL = os.environ.get("RAG_MODEL", "BAAI/bge-small-en-v1.5")

# Extensions we can pull plain text out of directly.
TEXT_EXTS = {
    ".txt", ".md", ".markdown", ".rst", ".log", ".csv", ".tsv", ".json",
    ".yaml", ".yml", ".toml", ".ini", ".cfg", ".html", ".htm", ".xml",
    ".py", ".go", ".js", ".ts", ".tsx", ".jsx", ".sh", ".bash", ".zsh",
    ".c", ".h", ".cpp", ".hpp", ".rs", ".rb", ".java", ".kt", ".swift",
    ".sql", ".env", ".conf", ".tex",
}
CHUNK_CHARS = 1000
CHUNK_OVERLAP = 150


# --------------------------------------------------------------------------- db
def connect(index_path):
    Path(index_path).parent.mkdir(parents=True, exist_ok=True)
    db = sqlite3.connect(index_path)
    db.execute("PRAGMA journal_mode=WAL")
    db.executescript(
        """
        CREATE TABLE IF NOT EXISTS sources(
            path   TEXT PRIMARY KEY,
            kind   TEXT NOT NULL,
            mtime  REAL,
            size   INTEGER,
            cursor INTEGER DEFAULT 0
        );
        CREATE TABLE IF NOT EXISTS chunks(
            id        INTEGER PRIMARY KEY,
            source    TEXT NOT NULL,
            kind      TEXT NOT NULL,
            ref       TEXT,
            text      TEXT NOT NULL,
            embedding BLOB
        );
        CREATE INDEX IF NOT EXISTS chunks_source ON chunks(source);
        """
    )
    return db


def pack(vec):
    return struct.pack("<%df" % len(vec), *vec)


def unpack(blob):
    n = len(blob) // 4
    return struct.unpack("<%df" % n, blob)


# ----------------------------------------------------------------- text + chunks
def chunk_text(text):
    """Split text into ~CHUNK_CHARS windows on paragraph boundaries, with overlap."""
    text = text.strip()
    if not text:
        return []
    paras = [p.strip() for p in text.split("\n\n") if p.strip()]
    chunks, buf = [], ""
    for p in paras:
        if len(buf) + len(p) + 2 <= CHUNK_CHARS:
            buf = (buf + "\n\n" + p) if buf else p
            continue
        if buf:
            chunks.append(buf)
        # A single oversized paragraph: hard-split it.
        while len(p) > CHUNK_CHARS:
            chunks.append(p[:CHUNK_CHARS])
            p = p[CHUNK_CHARS - CHUNK_OVERLAP:]
        buf = p
    if buf:
        chunks.append(buf)
    return chunks


def extract_file(path):
    ext = Path(path).suffix.lower()
    if ext in TEXT_EXTS:
        return Path(path).read_text(encoding="utf-8", errors="ignore")
    if ext == ".pdf":
        try:
            from pypdf import PdfReader
            return "\n\n".join((pg.extract_text() or "") for pg in PdfReader(path).pages)
        except Exception as e:  # noqa: BLE001
            print(f"  ! pdf extract failed {path}: {e}", file=sys.stderr)
            return ""
    if ext == ".docx":
        try:
            import docx
            return "\n\n".join(p.text for p in docx.Document(path).paragraphs)
        except Exception as e:  # noqa: BLE001
            print(f"  ! docx extract failed {path}: {e}", file=sys.stderr)
            return ""
    return ""  # images / audio / unknown binary → skip (audio content is in the transcript)


# Harness/tooling boilerplate that carries no recall value — skip these turns so
# embeddings capture real conversation, not notification spam.
NOISE_PREFIXES = (
    "<task-notification", "<system-reminder", "[system notification",
    "<local-command", "<command-name>", "<command-message>", "caveat:",
    "this session is being continued", "[request interrupted",
)


def is_noise(text):
    low = text.lstrip().lower()
    return any(low.startswith(p) for p in NOISE_PREFIXES)


def transcript_turns(raw_bytes):
    """Yield 'ROLE: text' strings from complete JSONL lines; return consumed byte count."""
    last_nl = raw_bytes.rfind(b"\n")
    consumed = last_nl + 1 if last_nl >= 0 else 0
    out = []
    for line in raw_bytes[:consumed].splitlines():
        if not line.strip():
            continue
        try:
            rec = json.loads(line)
        except Exception:  # noqa: BLE001
            continue
        msg = rec.get("message") or {}
        role = msg.get("role") or rec.get("type")
        if role not in ("user", "assistant"):
            continue
        content = msg.get("content")
        text = ""
        if isinstance(content, str):
            text = content
        elif isinstance(content, list):
            text = "\n".join(b.get("text", "") for b in content
                              if isinstance(b, dict) and b.get("type") == "text")
        text = text.strip()
        if text and not is_noise(text):
            out.append(f"{role.upper()}: {text}")
    return out, consumed


# ------------------------------------------------------------------- index pass
def add_chunks(db, model, source, kind, ref, texts):
    if not texts:
        return 0
    embs = list(model.embed(texts))
    rows = []
    for t, e in zip(texts, embs):
        v = e.tolist() if hasattr(e, "tolist") else list(e)
        rows.append((source, kind, ref, t, pack(v)))
    db.executemany(
        "INSERT INTO chunks(source, kind, ref, text, embedding) VALUES (?,?,?,?,?)", rows
    )
    return len(rows)


def index_file(db, model, path):
    st = os.stat(path)
    row = db.execute("SELECT mtime, size FROM sources WHERE path=?", (path,)).fetchone()
    if row and row[0] == st.st_mtime and row[1] == st.st_size:
        return 0
    text = extract_file(path)
    db.execute("DELETE FROM chunks WHERE source=?", (path,))  # re-index cleanly if changed
    n = add_chunks(db, model, path, "attachment", Path(path).name, chunk_text(text))
    db.execute(
        "INSERT INTO sources(path, kind, mtime, size, cursor) VALUES (?,?,?,?,0) "
        "ON CONFLICT(path) DO UPDATE SET mtime=excluded.mtime, size=excluded.size",
        (path, "attachment", st.st_mtime, st.st_size),
    )
    return n


def index_transcript(db, model, path):
    st = os.stat(path)
    row = db.execute("SELECT cursor FROM sources WHERE path=?", (path,)).fetchone()
    cursor = row[0] if row else 0
    if cursor >= st.st_size:
        return 0
    with open(path, "rb") as f:
        f.seek(cursor)
        raw = f.read()
    turns, consumed = transcript_turns(raw)
    ref = Path(path).stem  # session id
    texts = []
    for t in turns:
        texts.extend(chunk_text(t))
    n = add_chunks(db, model, path, "conversation", ref, texts)
    db.execute(
        "INSERT INTO sources(path, kind, mtime, size, cursor) VALUES (?,?,?,?,?) "
        "ON CONFLICT(path) DO UPDATE SET mtime=excluded.mtime, size=excluded.size, cursor=excluded.cursor",
        (path, "conversation", st.st_mtime, st.st_size, cursor + consumed),
    )
    return n


def cmd_index(args):
    model = load_model(args.model)
    db = connect(args.index)
    total = 0
    if args.scope in ("both", "attachments"):
        for p in sorted(glob.glob(os.path.join(args.inbox, "**", "*"), recursive=True)):
            if os.path.isfile(p) and os.path.basename(p) != "INDEX.md":
                try:
                    total += index_file(db, model, p)
                except Exception as e:  # noqa: BLE001
                    print(f"  ! skip {p}: {e}", file=sys.stderr)
    if args.scope in ("both", "conversations"):
        pattern = str(DEFAULT_PROJECTS / "*" / "*.jsonl")
        if args.project_only:
            slug = str(Path(os.getcwd()).resolve()).replace("/", "-")
            pattern = str(DEFAULT_PROJECTS / slug / "*.jsonl")
        for p in sorted(glob.glob(pattern)):
            try:
                total += index_transcript(db, model, p)
            except Exception as e:  # noqa: BLE001
                print(f"  ! skip {p}: {e}", file=sys.stderr)
    db.commit()
    print(f"indexed {total} new chunk(s)")
    return 0


# ------------------------------------------------------------------------ query
def cmd_query(args):
    import numpy as np

    model = load_model(args.model)
    db = connect(args.index)
    rows = db.execute("SELECT ref, kind, text, embedding FROM chunks").fetchall()
    if not rows:
        print("(index is empty — run `rag index` first)")
        return 0
    mat = np.array([unpack(r[3]) for r in rows], dtype=np.float32)
    mat /= (np.linalg.norm(mat, axis=1, keepdims=True) + 1e-9)
    qv = list(model.query_embed([args.text]))[0]
    qv = np.asarray(qv, dtype=np.float32)
    qv /= (np.linalg.norm(qv) + 1e-9)
    scores = mat @ qv
    k = min(args.k, len(rows))
    top = np.argsort(-scores)[:k]

    if args.json:
        out = [{"score": round(float(scores[i]), 4), "kind": rows[i][1],
                "ref": rows[i][0], "text": rows[i][2]} for i in top]
        print(json.dumps(out, ensure_ascii=False, indent=2))
        return 0
    for rank, i in enumerate(top, 1):
        ref, kind, text = rows[i][0], rows[i][1], rows[i][2]
        snippet = text if len(text) <= 600 else text[:600] + "…"
        print(f"[{rank}] score={scores[i]:.3f}  {kind}  «{ref}»")
        print(snippet)
        print("-" * 60)
    return 0


def cmd_stats(args):
    db = connect(args.index)
    n_src = db.execute("SELECT COUNT(*) FROM sources").fetchone()[0]
    n_chunk = db.execute("SELECT COUNT(*) FROM chunks").fetchone()[0]
    by_kind = db.execute("SELECT kind, COUNT(*) FROM chunks GROUP BY kind").fetchall()
    print(f"sources: {n_src}\nchunks:  {n_chunk}")
    for kind, c in by_kind:
        print(f"  {kind}: {c}")
    return 0


# ------------------------------------------------------------------------ model
def load_model(name):
    from fastembed import TextEmbedding
    return TextEmbedding(model_name=name)


def main():
    ap = argparse.ArgumentParser(description="Local RAG over attachments + conversations")
    ap.add_argument("--index", default=str(DEFAULT_INDEX))
    ap.add_argument("--model", default=DEFAULT_MODEL)
    sub = ap.add_subparsers(dest="cmd", required=True)

    pi = sub.add_parser("index")
    pi.add_argument("--scope", choices=["both", "attachments", "conversations"], default="both")
    pi.add_argument("--inbox", default=DEFAULT_INBOX)
    pi.add_argument("--project-only", action="store_true")
    pi.set_defaults(func=cmd_index)

    pq = sub.add_parser("query")
    pq.add_argument("text")
    pq.add_argument("-k", type=int, default=6)
    pq.add_argument("--json", action="store_true")
    pq.set_defaults(func=cmd_query)

    ps = sub.add_parser("stats")
    ps.set_defaults(func=cmd_stats)

    args = ap.parse_args()
    return args.func(args)


if __name__ == "__main__":
    raise SystemExit(main())
