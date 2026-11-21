"""Paying out to connected accounts. PROVIDER.md describes the payouts endpoint."""

import json

import transport

BASE = "https://pay.example.com/v1"
ATTEMPTS = 3


class PayoutError(Exception):
    pass


def send_payout(account_id, amount_cents):
    """Pay amount_cents to account_id and return the provider's payout id.

    A timeout is tried again, up to ATTEMPTS in all.
    """
    for attempt in range(ATTEMPTS):
        try:
            resp = transport.post(f"{BASE}/payouts", {"account": account_id, "amount_cents": amount_cents})
        except TimeoutError:
            if attempt == ATTEMPTS - 1:
                raise PayoutError("payout timed out") from None
            continue
        if resp.status != 200:
            raise PayoutError(f"payout failed: {resp.status}")
        return json.loads(resp.body)["id"]
