---
name: project-tracker
description: >-
  Track the project directories worked in across session switches and resolve a
  project by (partial) name via wildcard search. TRIGGER when the user refers to
  a project by name ("switch to the bridge project", "what was that notes app",
  "let's work on foo"), asks which projects exist, or after a /project switch.
  Backed by a registry the bridge shares, so the list survives the session
  restart a /project switch causes.
---

# Project tracker

The assistant can switch its working project at runtime with `/project` (which
restarts the session in a new workdir — a *fresh conversation*, so the new
session starts with no memory of what came before). This skill keeps a
**persistent registry of project directories** so that survives the switch, and
lets a project be named loosely and resolved by **wildcard search**.

## The registry (serialize down / up)

- Store: `~/.local/share/assistant/projects.tsv` (override with
  `$CLAUDECGWD_PROJECTS`). One line per project: `<absolute-path>\t<last-used>`.
- **Serialized "down":** the bridge records every `/project` switch and the
  startup workdir into this file automatically. You can also record a directory
  you discover mid-task with `projects.sh record <dir>`.
- **Serialized "up":** because it's on disk, the *next* session (after a switch
  or restart) reads the same list — that's how tracking spans session switches.

Helper (bundled next to this file): `./projects.sh`
```sh
projects.sh record  <dir>      # remember a directory (upsert, stamp last-used)
projects.sh list               # tracked dirs, most-recently-used first
projects.sh resolve <pattern>  # wildcard-match the registry; if nothing matches,
                               # fall back to a filesystem search under $HOME
```

## When the user names a project

Resolving by **wildcard is the default** — don't require an exact path.

1. Run `projects.sh resolve "<name>"` (the script's dir; it's executable).
2. On the result:
   - **one match** → that's the project. If they want to switch, tell them to
     send `/project <name>` (the `/project` command wildcard-resolves the same
     way), or just use the resolved absolute path to answer their question.
   - **several matches** → list them briefly and ask which one (or suggest a
     more specific name).
   - **no registry match** → the script falls back to a filesystem wildcard
     search under `$HOME`; offer the candidates it finds, and `record` the one
     they pick so it's tracked next time.
3. If you end up working in a project that isn't tracked yet, `record` it.

## Switching

- `/project <name|dir>` is the actual switch (handled by the bridge, not by you
  — it restarts the session). A bare name is wildcard-matched against the
  registry; a path with `/` or `~` is taken literally.
- You **cannot** issue `/project` yourself (it would restart you mid-turn). When
  a switch is wanted, resolve the name and hand the user the exact
  `/project <name>` to send.
- `/projects` lists the tracked set; `/status` shows the current one.

## Notes

- The registry and the `/project` command read the **same** file — keep using
  the helper (don't invent a parallel list).
- Matching is case-insensitive substring by default; `*`/`?` globs work too.
- Best-effort: a failed record never blocks anything. If the store is missing
  it's simply empty until the first switch or `record`.
