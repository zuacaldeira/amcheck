#!/usr/bin/env bash
# GitHub Action entrypoint: diff the Alertmanager config between the PR base and
# head with amcheck, annotate the PR, and fail the check on a regression.
set -uo pipefail

CONFIG="${INPUT_CONFIG:?config input is required}"
MODE="${INPUT_MODE:-operational}"
COMMENT="${INPUT_COMMENT:-true}"

# The amcheck Go module is the parent of this action directory.
AMDIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$(mktemp -u)"
echo "::group::build amcheck"
if ! go build -C "$AMDIR" -o "$BIN" . ; then
  echo "::error::failed to build amcheck"; exit 1
fi
echo "::endgroup::"

if [ ! -f "$CONFIG" ]; then
  echo "::error::config file not found: $CONFIG"; exit 1
fi

# Resolve the base version of the config.
base_spec="${INPUT_BASE_REF:-}"
if [ -z "$base_spec" ]; then
  if [ -n "${GITHUB_BASE_REF:-}" ]; then           # pull_request event
    git fetch --quiet origin "$GITHUB_BASE_REF" 2>/dev/null || true
    base_spec="origin/$GITHUB_BASE_REF"
  else                                             # push event
    base_spec="HEAD^"
  fi
fi

tmpd="$(mktemp -d)"; base_yml="$tmpd/base.yml"; head_yml="$tmpd/head.yml"
cp "$CONFIG" "$head_yml"
if ! git show "$base_spec:$CONFIG" > "$base_yml" 2>/dev/null; then
  echo "::notice::$CONFIG has no version at $base_spec (new file?) — nothing to diff"
  exit 0
fi

args=(--mode "$MODE")
[ -n "${INPUT_OLD_SILENCES:-}" ] && args+=(--old-silences "$INPUT_OLD_SILENCES")
[ -n "${INPUT_NEW_SILENCES:-}" ] && args+=(--new-silences "$INPUT_NEW_SILENCES")

out="$("$BIN" "$base_yml" "$head_yml" --explain "${args[@]}" 2>&1)"; ec=$?
echo "$out"

# Job summary (renders on the run page).
{
  echo "### amcheck — \`$CONFIG\`  (base \`$base_spec\`)"
  echo ""
  echo '```'
  echo "$out"
  echo '```'
} >> "${GITHUB_STEP_SUMMARY:-/dev/null}"

# Optional PR comment (best effort; needs pull-requests: write).
if [ "$COMMENT" = "true" ] && [ -n "${GITHUB_BASE_REF:-}" ] && command -v gh >/dev/null; then
  body="$(printf '**amcheck** on \`%s\`\n\n```\n%s\n```' "$CONFIG" "$out")"
  gh pr comment "${GITHUB_HEAD_REF:-}" --body "$body" 2>/dev/null \
    || gh pr comment "$(gh pr view --json number -q .number 2>/dev/null)" --body "$body" 2>/dev/null \
    || echo "::notice::could not post PR comment (grant pull-requests: write to enable)"
fi

case $ec in
  0) echo "::notice::amcheck: no route regression or new suppression in $CONFIG"; exit 0 ;;
  1) echo "::error::amcheck: route regression or new suppression in $CONFIG — see the check output"; exit 1 ;;
  *) echo "::error::amcheck could not evaluate $CONFIG (exit $ec)"; exit 1 ;;
esac
