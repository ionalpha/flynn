# Proof walkthrough

Two scripts that show what a sealed run record proves, and what the verifier refuses to
accept. Every verdict below is unedited `flynn spine verify` output.

## What it shows

A correctly signed record can still be rejected. The verifier reports three things about
a record, and a signature covers only the first:

- **integrity**: the events rebuild the signed Merkle root (any change breaks it),
- **governance**: no action ran without a preceding admission,
- **ground truth**: a claimed success is backed by an independent check.

## `proof.sh` (no model required)

Runs the verifier against the published conformance vectors. The valid records pass; the
signed-but-invalid records are rejected with the exact failure code. It needs nothing but
the repository:

```
$ ./demo/proof.sh

1. A real run record. Integrity holds, it was governed, no outcome was claimed.
  integrity:    VERIFIED (3 events, signed by provetrail-conformance-root)
  governance:   OK (no action ran without admission)
  ground-truth: not asserted (no independent check was bound)

2. A run whose success is backed by an independent check. This is PROVEN, not just signed.
  integrity:    VERIFIED (2 events, signed by provetrail-conformance-root)
  governance:   OK (no action ran without admission)
  ground-truth: GROUNDED (success backed by a passing check)

3. A perfectly SIGNED record where an action ran with no admission. Caught.
  integrity:    VERIFIED (3 events, signed by provetrail-conformance-root)
  governance:   VIOLATION: gov.unadmitted_action: chain: an action completed with no preceding admission

4. A perfectly SIGNED record claiming success with nothing behind it.
  integrity:    VERIFIED (1 events, signed by provetrail-conformance-root)
  governance:   OK (no action ran without admission)
  ground-truth: NOT GROUNDED: shallow.no_ground_truth: chain: a success outcome is not grounded in a passing check

5. A record with a single byte changed. The proof breaks.
  integrity:    NOT VERIFIED: enc.invalid_utf8: cbor: invalid UTF-8 string
```

Steps 3-5 are all signed by a key the verifier trusts, and all rejected. The command
exits non-zero on any failed tier, so it gates a script.

## `proof-live.sh` (drives a live agent)

Drives two goals with a live model and verifies their sealed records. One is given a check it
satisfies (grounded); the other claims success while its independent check disagrees
(not grounded).

It needs a model: store a provider key once (`flynn auth set anthropic`) or run fully
local with no key (`flynn models use <id>`, see `flynn models --local`).

```
$ ./demo/proof-live.sh [--model provider:model]

A. A goal whose success is checked independently and holds: GROUNDED.
  run 019f148e-144e-7084-b209-35854f32bc69
  Created `status.txt` containing exactly `READY`.

B. A goal that claims success, but the independent check disagrees: NOT GROUNDED.
  run 019f148e-21da-765f-8936-9f4900c323c8
  The release is complete.

Both runs are sealed. Verify them from the durable store, tier by tier:
run 019f148e-144e-7084-b209-35854f32bc69
  integrity:    VERIFIED (17 events, signed by ed25519:6Uugkf0Tj2N5ZSAm3n1U6uEJ8sgbv4rR6US0GF6zd5M)
  governance:   OK (no action ran without admission)
  ground-truth: GROUNDED (success backed by a passing check)

run 019f148e-21da-765f-8936-9f4900c323c8
  integrity:    VERIFIED (9 events, signed by ed25519:6Uugkf0Tj2N5ZSAm3n1U6uEJ8sgbv4rR6US0GF6zd5M)
  governance:   OK (no action ran without admission)
  ground-truth: NOT GROUNDED: shallow.no_ground_truth: chain: a success outcome is not grounded in a passing check
```

Run B is the agent reporting success that did not happen. It is signed and governed; the
ground-truth check is what rejects it. The verification command runs in the platform shell (sh on Unix, cmd on
Windows), so the script picks a check the local shell understands.

## Where the records come from

The forged records in `proof.sh` (steps 3-5) are conformance vectors, each made by
changing a valid record in exactly one way. The valid records come from the production
signing path. `proof-live.sh` drives a live agent end to end.
