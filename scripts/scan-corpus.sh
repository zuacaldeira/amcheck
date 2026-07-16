#!/usr/bin/env bash
# Batch-scan a list of "owner/repo  path" candidates: run amcheck across each
# config's git history and aggregate regression / safe / skipped counts into a
# report. Public data only.
#
#   scan-corpus.sh <candidates-file> [report.md]
set -uo pipefail

CANDIDATES="${1:?usage: scan-corpus.sh <candidates-file> [report.md]}"
REPORT="${2:-scan-report.md}"
HARNESS="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/mine-history.sh"

repos=0; withhist=0; pairs=0; regr=0; safe=0; skip=0
details=""; regrepos=0

while read -r repo path _; do
  [ -z "$repo" ] && continue
  repos=$((repos+1))
  out="$(timeout 150 bash "$HARNESS" "$repo" "$path" 2>/dev/null)"
  line="$(printf '%s\n' "$out" | grep -oE '[0-9]+ regression, [0-9]+ safe, [0-9]+ skipped')"
  if [ -n "$line" ]; then
    withhist=$((withhist+1))
    r=$(printf '%s' "$line" | sed -E 's/([0-9]+) regression.*/\1/')
    s=$(printf '%s' "$line" | sed -E 's/.*, ([0-9]+) safe.*/\1/')
    k=$(printf '%s' "$line" | sed -E 's/.*, ([0-9]+) skipped/\1/')
    regr=$((regr+r)); safe=$((safe+s)); skip=$((skip+k)); pairs=$((pairs+r+s+k))
    if [ "$r" -gt 0 ]; then
      regrepos=$((regrepos+1))
      details+="### [$repo](https://github.com/$repo) — \`$path\`"$'\n\n'
      details+='```'$'\n'
      details+="$(printf '%s\n' "$out" | grep -E 'REGRESSION|no longer reachable|can mute it|witness|→ ')"$'\n'
      details+='```'$'\n\n'
    fi
    echo "[$repos] $repo :: $line"
  else
    echo "[$repos] $repo :: (single revision / unusable)"
  fi
done < "$CANDIDATES"

{
  echo "# amcheck — public-corpus scan"
  echo ""
  echo "Ran the git-history harness over public GitHub repos containing an"
  echo "Alertmanager config, diffing every consecutive revision with amcheck."
  echo "Public data only."
  echo ""
  echo "## Results"
  echo ""
  echo "| Metric | Count |"
  echo "|---|---|"
  echo "| Repos scanned | $repos |"
  echo "| Repos with >= 2 config revisions | $withhist |"
  echo "| Revision pairs diffed | $pairs |"
  echo "| **Route regressions / new suppressions found** | **$regr** (in $regrepos repos) |"
  echo "| Safe changes (correctly passed) | $safe |"
  echo "| Skipped (one side unparseable) | $skip |"
  echo ""
  echo "## Regressions found"
  echo ""
  echo "${details:-_None found in this batch._}"
} > "$REPORT"
echo "=== wrote $REPORT :: $regr regression(s) across $regrepos repo(s), $withhist repos had history ==="
