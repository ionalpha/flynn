#!/usr/bin/env bash
#
# A walkthrough of what a Provetrail record proves, and what it refuses to accept.
# Every step runs the real `flynn spine verify` against the published conformance
# vectors, so the output is the reference verifier's actual verdict, not a mock.
#
# Run from the repository root:
#   ./demo/proof.sh
#
# It builds the binary, then verifies one record per claim. The signed-but-invalid
# records (steps 3-5) are the point: a record can be perfectly signed and still be
# rejected, because being signed is not the same as being proven.

set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"

flynn="$root/flynn"
echo "building flynn..."
go build -o "$flynn" ./cmd/flynn

vectors="chain/conformance/testdata/crypto"
# The vectors are signed by a fixed, published test key (not for production); the
# verifier needs its public half, which the suite manifest carries.
key="$(grep -oE '[0-9a-f]{64}' "$vectors/manifest.json" | head -1)"

hr() { printf '\n\033[1m%s\033[0m\n' "$*"; }
show() { "$flynn" spine verify --file "$1" --key "$key" || true; }

hr "1. A real run record. Integrity holds, it was governed, no outcome was claimed."
show "$vectors/valid/crypto_run_valid_01.cbor"

hr "2. A run whose success is backed by an independent check. This is PROVEN, not just signed."
show "$vectors/valid/crypto_ground_truth_valid_01.cbor"

hr "3. A perfectly SIGNED record where an action ran with no admission. Caught."
show "$vectors/invalid/crypto_governance_unadmitted_action_01.cbor"

hr "4. A perfectly SIGNED record claiming success with nothing behind it. The difference between signed and proven."
show "$vectors/invalid/crypto_ground_truth_unbound_success_01.cbor"

hr "5. A record with a single byte changed. The proof breaks."
tampered="$(mktemp)"
cp "$vectors/valid/crypto_run_valid_01.cbor" "$tampered"
printf '\xff' | dd of="$tampered" bs=1 seek=20 count=1 conv=notrunc 2>/dev/null
show "$tampered"
rm -f "$tampered"

hr "Done. Steps 3-5 were all signed by a key the verifier trusts, and all rejected."
