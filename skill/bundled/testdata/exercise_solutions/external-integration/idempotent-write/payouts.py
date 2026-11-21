"""Paying out to connected accounts. PROVIDER.md describes the payouts endpoint."""

import json
import uuid

import transport

BASE = "https://pay.example.com/v1"
ATTEMPTS = 3


class PayoutError(Exception):
    pass


def send_payout(account_id, amount_cents):
    """Pay amount_cents to account_id and return the provider's payout id.

    The idempotency key is made once per payout, before the first attempt, so a retry
    after a timeout replays the payout the provider may already have made instead of
    making a second one. A timeout is tried again, up to ATTEMPTS in all.
    """
    headers = {"Idempotency-Key": uuid.uuid4().hex}
    for attempt in range(ATTEMPTS):
        try:
            resp = transport.post(f"{BASE}/payouts", {"account": account_id, "amount_cents": amount_cents}, headers)
        except TimeoutError:
            if attempt == ATTEMPTS - 1:
                raise PayoutError("payout timed out; it may have been made, retry with the same key to find out") from None
            continue
        if resp.status != 200:
            raise PayoutError(f"payout failed: {resp.status}")
        return json.loads(resp.body)["id"]
