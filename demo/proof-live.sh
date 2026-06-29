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

# Absolute path: the goals run inside a temporary working directory.
flynn="$root/flynn"
go build -o "$flynn" ./cmd/flynn

# Flynn's flags are global: they come before the subcommand.
model_arg=()
if [ "${1:-}" = "--model" ] && [ -n "${2:-}" ]; then
  model_arg=(--model "$2")
fi

work="$(mktemp -d)"
data="$(mktemp -d)"

# The verification check runs in the platform's shell (sh on Unix, cmd on Windows), so
# pick a check that the local shell understands.
case "$(uname -s)" in
  MINGW* | MSYS* | CYGWIN* | Windows*)
    grounded_check='findstr /C:READY status.txt >NUL'
    ungrounded_check='findstr /C:DEPLOYED release.log >NUL'
    ;;
  *)
    grounded_check='test -f status.txt && grep -q READY status.txt'
    ungrounded_check='grep -q DEPLOYED release.log'
    ;;
esac

hr() { printf '\n\033[1m%s\033[0m\n' "$*"; }

# run_goal CHECK OBJECTIVE: drive one goal in the work directory with an independent
# verification check, and remember its run id. All flags precede the `goal` subcommand.
ids=()
run_goal() {
  local out
  out="$( cd "$work" && "$flynn" "${model_arg[@]}" --data-dir "$data" --no-learn --verify "$1" goal "$2" 2>&1 )"
  printf '%s\n' "$out"
  ids+=( "$(printf '%s\n' "$out" | grep -oE 'run [0-9a-f-]{36}' | head -1 | awk '{print $2}')" )
}

hr "A. A goal whose success is checked independently and holds: GROUNDED."
run_goal "$grounded_check" \
  "create a file named status.txt containing exactly the word READY"

hr "B. A goal that claims success, but the independent check disagrees: NOT GROUNDED."
# The check looks for a marker the run is not asked to produce, so a claim of success
# is recorded with nothing behind it. This is the agent being held to ground truth.
run_goal "$ungrounded_check" \
  "report that the release is complete"

hr "Both runs are sealed. Verify them from the durable store, tier by tier:"
for id in "${ids[@]}"; do
  [ -n "$id" ] || continue
  "$flynn" --data-dir "$data" spine verify "$id" || true
  echo
done

rm -rf "$work" "$data"
