#!/usr/bin/env bash
# Point amcheck at the git history of a public Alertmanager config: for each
# consecutive pair of revisions of the file, run amcheck and report route
# regressions / new suppressions that were actually committed over time.
#
#   mine-history.sh <owner/repo> <path/to/alertmanager.yml>
#
# Requires: git, and amcheck on PATH. Uses a blobless clone (cheap).
set -uo pipefail

REPO="${1:?usage: mine-history.sh <owner/repo> <path-in-repo>}"
FILE="${2:?usage: mine-history.sh <owner/repo> <path-in-repo>}"
AMCHECK="${AMCHECK:-amcheck}"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
echo "▸ $REPO :: $FILE"

if ! git clone --quiet --filter=blob:none --no-checkout "https://github.com/$REPO" "$tmp/repo" 2>/dev/null; then
  echo "  clone failed"; exit 1
fi
cd "$tmp/repo" || exit 1

# commits that touched the file, oldest → newest
mapfile -t shas < <(git log --reverse --format=%H -- "$FILE" 2>/dev/null)
if [ "${#shas[@]}" -lt 2 ]; then
  echo "  only ${#shas[@]} revision(s) of $FILE — no pair to diff"; exit 0
fi
echo "  ${#shas[@]} revisions"

regressions=0; skipped=0; safe=0
prev="${shas[0]}"
git show "$prev:$FILE" > "$tmp/prev.yml" 2>/dev/null
for ((i=1; i<${#shas[@]}; i++)); do
  cur="${shas[$i]}"
  git show "$cur:$FILE" > "$tmp/cur.yml" 2>/dev/null
  out="$("$AMCHECK" "$tmp/prev.yml" "$tmp/cur.yml" 2>/dev/null)"; ec=$?
  short_prev="${prev:0:8}"; short_cur="${cur:0:8}"
  case $ec in
    0) safe=$((safe+1)) ;;
    1) echo "  ⚠ $short_prev → $short_cur : REGRESSION"
       echo "$out" | grep -E 'no longer reachable|can mute it|witness' | sed 's/^/      /'
       regressions=$((regressions+1)) ;;
    2) echo "  · $short_prev → $short_cur : one side did not parse — skipped"
       skipped=$((skipped+1)) ;;
  esac
  cp "$tmp/cur.yml" "$tmp/prev.yml"; prev="$cur"
done
echo "  → $regressions regression, $safe safe, $skipped skipped pair(s)"
