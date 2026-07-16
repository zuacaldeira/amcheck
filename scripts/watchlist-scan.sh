#!/usr/bin/env bash
# Daily watchlist scan. For each watched public Alertmanager config, detect
# whether it changed since the last run (by the file's latest-commit SHA) and,
# if so, diff old -> new with amcheck. Updates the state file and an
# AGGREGATE-ONLY scoreboard. Uses the GitHub API (no clone). Public data only.
#
# By design the scoreboard never names a repo: we monitor public configs, we do
# not publish a list of "who has a regression". Individual repos stay unnamed.
#
# Env: WATCHLIST, STATE, SCOREBOARD, AMCHECK, SCAN_DATE (override for tests).
set -uo pipefail
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

WATCHLIST="${WATCHLIST:-watchlist.txt}"
STATE="${STATE:-watchlist-state.tsv}"
SCOREBOARD="${SCOREBOARD:-SCOREBOARD.md}"
AMCHECK="${AMCHECK:-amcheck}"
DATE="${SCAN_DATE:-$(date -u +%F)}"

touch "$STATE"
tmpnew="$(mktemp)"; tmpa="$(mktemp)"; tmpb="$(mktemp)"
trap 'rm -f "$tmpnew" "$tmpa" "$tmpb"' EXIT

d_changes=0; d_flagged=0; d_safe=0; d_skip=0; d_new=0; watched=0

fetch_sha()  { gh api "repos/$1/commits?path=$2&per_page=1" --jq '.[0].sha' 2>/dev/null; }
fetch_file() { gh api "repos/$1/contents/$2?ref=$3" --jq '.content' 2>/dev/null | tr -d '\n' | base64 -d 2>/dev/null; }
lookup()     { awk -F'\t' -v k="$1" '$1==k{print $2}' "$STATE"; }

while read -r repo path _; do
  [ -z "$repo" ] && continue
  case "$repo" in \#*) continue ;; esac
  watched=$((watched+1))
  key="$repo|$path"
  cur="$(fetch_sha "$repo" "$path")"
  prev="$(lookup "$key")"

  if [ -z "$cur" ] || [ "$cur" = "null" ]; then      # unreachable this run — keep prior
    printf '%s\t%s\n' "$key" "${prev:-unknown}" >> "$tmpnew"; continue
  fi
  if [ -z "$prev" ] || [ "$prev" = "unknown" ]; then # first sighting — baseline, no diff
    d_new=$((d_new+1)); printf '%s\t%s\n' "$key" "$cur" >> "$tmpnew"; continue
  fi
  if [ "$prev" = "$cur" ]; then                      # unchanged
    printf '%s\t%s\n' "$key" "$cur" >> "$tmpnew"; continue
  fi

  # changed since last run — diff it
  d_changes=$((d_changes+1))
  fetch_file "$repo" "$path" "$prev" > "$tmpa"
  fetch_file "$repo" "$path" "$cur"  > "$tmpb"
  "$AMCHECK" "$tmpa" "$tmpb" >/dev/null 2>&1
  case $? in
    1) d_flagged=$((d_flagged+1)) ;;
    0) d_safe=$((d_safe+1)) ;;
    *) d_skip=$((d_skip+1)) ;;
  esac
  printf '%s\t%s\n' "$key" "$cur" >> "$tmpnew"
done < "$WATCHLIST"

sort -o "$STATE" "$tmpnew"

# cumulative totals live in an HTML comment at the top of the scoreboard
tot() { grep -oE "$1=[0-9]+" "$SCOREBOARD" 2>/dev/null | head -1 | grep -oE '[0-9]+'; }
c_runs=$(( $(tot runs || echo 0)     + 1 ))
c_chg=$((  $(tot changes || echo 0)  + d_changes ))
c_flg=$((  $(tot flagged || echo 0)  + d_flagged ))
c_safe=$(( $(tot safe || echo 0)     + d_safe ))
c_skip=$(( $(tot skipped || echo 0)  + d_skip ))

prior_log="$(sed -n '/^## Daily log/,$p' "$SCOREBOARD" 2>/dev/null | tail -n +3)"
{
  echo "<!-- totals: runs=$c_runs changes=$c_chg flagged=$c_flg safe=$c_safe skipped=$c_skip -->"
  echo "# amcheck watchlist scoreboard"
  echo ""
  echo "Automated daily scan of public Alertmanager configs with amcheck."
  echo "**Aggregate only** — individual repositories are never named here."
  echo ""
  echo "| Metric | Total |"
  echo "|---|---|"
  echo "| Configs watched | $watched |"
  echo "| Runs to date | $c_runs |"
  echo "| Config changes observed | $c_chg |"
  echo "| Changes flagged for review | $c_flg |"
  echo "| Changes passed as safe | $c_safe |"
  echo "| Changes skipped (unparseable / template) | $c_skip |"
  echo ""
  echo "## Daily log"
  echo ""
  echo "- **$DATE** — watched $watched, $d_new new baseline, $d_changes changed → $d_flagged flagged, $d_safe safe, $d_skip skipped"
  [ -n "$prior_log" ] && printf '%s\n' "$prior_log"
} > "$SCOREBOARD.tmp"
mv "$SCOREBOARD.tmp" "$SCOREBOARD"

echo "watchlist $DATE: watched=$watched new=$d_new changed=$d_changes flagged=$d_flagged safe=$d_safe skip=$d_skip"
