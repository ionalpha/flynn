"""Our side of acmepay. The rest of the code reads Charge, never the SDK's types."""

from dataclasses import dataclass

import acmepay

_client = acmepay.Client("sk_test")


@dataclass(frozen=True)
class Charge:
    id: str
    amount_cents: int
    currency: str


def list_charges(month="2026-10"):
    return [Charge(c.id, c.amount, c.currency.upper()) for c in _client.charges.list(month)]
