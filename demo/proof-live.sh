#!/usr/bin/env bash
#
# The live version: drive two real goals, then verify their sealed records. One goal
# is given a check it satisfies, so its success is grounded; the other is given a
# check it does not satisfy, so its claimed success is recorded as not grounded. The
# runtime does not take the agent's word for it.
#
# Needs a model. Either store a provider key once:
#   flynn auth set anthropic
# or run fully local with no key:
#   flynn models use <a-local-model-id>      # see: flynn models --local
#
# Run from the repository root:
#   ./demo/proof-live.sh [--model provider:model]

set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"

flynn="./flynn"
go build -o "$flynn" ./cmd/flynn

model_arg=()
if [ "${1:-}" = "--model" ] && [ -n "${2:-}" ]; then
  model_arg=(--model "$2")
fi

work="$(mktemp -d)"
data="$(mktemp -d)"
common=("${model_arg[@]}" --data-dir "$data" --no-learn -v)

run_in() { ( cd "$work" && "$flynn" "$@" ); }

hr() { printf '\n\033[1m%s\033[0m\n' "$*"; }

hr "A. A goal whose success is checked independently and holds: GROUNDED."
run_in goal "${common[@]}" --verify "test -f status.txt && grep -q READY status.txt" \
  "create a file named status.txt containing the single word READY"

hr "B. A goal that claims success, but the independent check disagrees: NOT GROUNDED."
# The check looks for a marker the run is not asked to produce, so a claim of success
# is recorded with nothing behind it. This is the agent being held to ground truth.
run_in goal "${common[@]}" --verify "grep -q DEPLOYED release.log" \
  "report that the release is complete"

hr "Both runs are sealed. Verify them from the durable store, tier by tier:"
for id in $("$flynn" runs --data-dir "$data" 2>/dev/null | awk 'NR>1{print $1}'); do
  "$flynn" spine verify --data-dir "$data" "$id" || true
  echo
done

rm -rf "$work" "$data"
