#!/usr/bin/env bash
# Project-directory registry: the shell side of the project-tracker skill.
# Reads/writes the SAME store the bridge uses, so directories tracked here and
# directories tracked by /project switches are one shared, serialized list.
#
#   projects.sh record <dir>      # remember <dir> (upsert, stamps last-used)
#   projects.sh list              # tracked dirs, most-recently-used first
#   projects.sh resolve <pattern> # wildcard-match the registry; fall back to a
#                                 # filesystem search under $HOME if none match
#
# Store: $CLAUDECGWD_PROJECTS or ~/.local/share/assistant/projects.tsv
#        one line per project: "<absolute-path>\t<RFC3339-last-used>"

set -euo pipefail

store="${CLAUDECGWD_PROJECTS:-$HOME/.local/share/assistant/projects.tsv}"
roots="${CLAUDECGWD_PROJECT_ROOTS:-$HOME}"

cmd="${1:-help}"
shift || true

record() {
  local dir
  dir="$(cd "${1:?usage: record <dir>}" 2>/dev/null && pwd)" || {
    echo "no such directory: $1" >&2
    return 1
  }
  mkdir -p "$(dirname "$store")"
  local tmp
  tmp="$(mktemp)"
  # Drop any existing row for this path, then append a fresh, current one.
  if [[ -f "$store" ]]; then
    awk -F'\t' -v p="$dir" '$1 != p' "$store" > "$tmp" || true
  fi
  printf '%s\t%s\n' "$dir" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >> "$tmp"
  mv "$tmp" "$store"
  echo "$dir"
}

list() {
  [[ -f "$store" ]] || return 0
  # RFC3339 (UTC, Z) sorts lexicographically = chronologically.
  sort -t$'\t' -k2,2 -r "$store" | cut -f1
}

resolve() {
  local pat="${1:?usage: resolve <pattern>}"
  # 1) Wildcard (case-insensitive substring) against the tracked registry.
  if [[ -f "$store" ]]; then
    local hits
    hits="$(list | grep -i -F -- "$pat" || true)"
    if [[ -n "$hits" ]]; then
      printf '%s\n' "$hits"
      return 0
    fi
  fi
  # 2) Nothing tracked matches — discover on disk by wildcard (default).
  #    Prefer real projects (a .git inside) but list plain dirs too.
  find "$roots" -maxdepth 3 -type d -iname "*$pat*" \
       -not -path '*/.*' -not -path '*/node_modules/*' 2>/dev/null | sort -u | head -20
}

case "$cmd" in
  record)  record "$@" ;;
  list)    list ;;
  resolve) resolve "$@" ;;
  *)
    sed -n '2,14p' "$0" | sed 's/^# \{0,1\}//'
    ;;
esac
