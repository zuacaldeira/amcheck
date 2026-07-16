#!/usr/bin/env bash
# Append new public Alertmanager configs to watchlist.txt via GitHub code search.
# GitHub's code-search rate limit is low (~10/min), so this gathers a little at
# a time — run it periodically (daily) to grow the list rather than all at once.
# Public data only; templated files (.j2/.tpl/...) are skipped.
#
#   gather-candidates.sh [query ...]         # native alertmanager.yml files
#   gather-candidates.sh --crd [query ...]   # AlertmanagerConfig CRDs (k8s-native)
set -uo pipefail
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WATCHLIST="${WATCHLIST:-watchlist.txt}"

AMCHECK="${AMCHECK:-amcheck}"
mode="native"
if [ "${1:-}" = "--crd" ]; then mode="crd"; shift; fi
queries=("$@")
verify=0

if [ "$mode" = "crd" ]; then
  # CRDs aren't named alertmanager.yml, and GitHub code search only reliably
  # AND-matches a single token here — search the bare term across YAML files.
  # Most hits merely mention AlertmanagerConfig (Helm values, docs), so verify
  # each candidate is a real instance amcheck accepts before adding it.
  [ ${#queries[@]} -eq 0 ] && queries=('AlertmanagerConfig')
  exts=(yaml yml)
  export EXCLUDE_RE='/templates/|/charts/'   # bias away from Helm templates
  search() { gh search code "$1" --extension "$2" --limit 100 --json repository,path 2>/dev/null; }
  verify=1
else
  [ ${#queries[@]} -eq 0 ] && queries=('inhibit_rules' 'route receiver severity' 'group_by receivers')
  exts=(yml yaml)
  export EXCLUDE_RE='^\b$'                    # matches nothing
  search() { gh search code "$1" --filename="alertmanager.$2" --limit 100 --json repository,path 2>/dev/null; }
fi

existing="$(grep -vE '^[[:space:]]*(#|$)' "$WATCHLIST" | awk '{print $1}' | sort -u)"
found="$(mktemp)"; trap 'rm -f "$found"' EXIT

for q in "${queries[@]}"; do
  for ext in "${exts[@]}"; do
    search "$q" "$ext" | python3 -c "
import sys,json,os,re
excl=re.compile(os.environ.get('EXCLUDE_RE','^\b\$'))
try: d=json.load(sys.stdin)
except: sys.exit()
for r in d:
    p=r['path']; repo=r['repository']['nameWithOwner']
    if any(p.endswith(x) for x in ('.j2','.tpl','.template','.tmpl','_backup','.base','.example','.dist','.sample')): continue
    if not p.endswith(('.yml','.yaml')): continue
    if excl.search(p): continue
    print(repo, p)
" >> "$found" || echo "note: query '$q' ($ext) hit a rate limit or returned nothing" >&2
  done
done

vtmp="$(mktemp)"; trap 'rm -f "$found" "$vtmp"' EXIT
is_instance() { # repo path -> 0 if a real AlertmanagerConfig instance amcheck accepts
  gh api "repos/$1/contents/$2" --jq '.content' 2>/dev/null | tr -d '\n' | base64 -d > "$vtmp" 2>/dev/null
  grep -q 'kind:[[:space:]]*AlertmanagerConfig' "$vtmp" 2>/dev/null || return 1
  "$AMCHECK" "$vtmp" "$vtmp" >/dev/null 2>&1
}

added=0
while read -r repo path; do
  [ -z "$repo" ] && continue
  grep -qxF "$repo" <<<"$existing" && continue
  if [ "$verify" = 1 ] && ! is_instance "$repo" "$path"; then continue; fi
  printf '%s %s\n' "$repo" "$path" >> "$WATCHLIST"
  existing="$existing"$'\n'"$repo"
  added=$((added+1))
done < <(sort -u "$found")

echo "[$mode] added $added new repos; watchlist now has $(grep -cvE '^[[:space:]]*(#|$)' "$WATCHLIST") entries"
