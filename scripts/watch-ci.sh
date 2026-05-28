#!/usr/bin/env bash
# Poll a GitHub Actions workflow run until it completes, then print the result.
#
# The repo is public, so this uses the unauthenticated REST API — no gh CLI and
# no token required. Lets the assistant actually watch CI and report back.
#
# Usage:
#   scripts/watch-ci.sh [SHA] [WORKFLOW_FILE]
# Defaults: latest run of publish-image.yml. If SHA is given, waits for the run
# matching that commit.
#
# Exit code: 0 if the run concluded "success", 1 otherwise (incl. timeout).

set -euo pipefail

REPO="${CI_REPO:-wildcommitter/claudecgwd}"
SHA="${1:-}"
WF="${2:-publish-image.yml}"
INTERVAL="${CI_POLL_INTERVAL:-15}"
TIMEOUT="${CI_POLL_TIMEOUT:-1800}" # 30 min

api="https://api.github.com/repos/${REPO}/actions/workflows/${WF}/runs?per_page=20"
deadline=$(( $(date +%s) + TIMEOUT ))
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

while :; do
  if curl -fsS "$api" -o "$tmp" 2>/dev/null; then
    # python reads the JSON from the temp file (argv[1]); the heredoc is the
    # program, so stdin stays free of the data.
    result="$(SHA="$SHA" python3 - "$tmp" <<'PY'
import json, os, sys
sha = os.environ.get("SHA", "")
try:
    runs = json.load(open(sys.argv[1])).get("workflow_runs", [])
except Exception:
    print("error none none none"); sys.exit()
run = None
if sha:
    for r in runs:
        if r["head_sha"].startswith(sha) or sha.startswith(r["head_sha"][:8]):
            run = r; break
elif runs:
    run = runs[0]
if not run:
    print("pending none none none")
else:
    print(run["status"], run["conclusion"] or "none", run["head_sha"][:8], run["html_url"])
PY
)"
    read -r status conclusion runsha url <<<"$result"

    if [[ "$status" == "completed" ]]; then
      echo "CI ${conclusion} (sha ${runsha}) ${url}"
      [[ "$conclusion" == "success" ]] && exit 0 || exit 1
    fi
  else
    status="unreachable"
  fi

  if (( $(date +%s) >= deadline )); then
    echo "CI watch timed out after ${TIMEOUT}s (last status: ${status})"
    exit 1
  fi
  sleep "$INTERVAL"
done
