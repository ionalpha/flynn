"""Our side of acmepay. The rest of the code reads Charge, never the SDK's types."""

from dataclasses import dataclass

import acmepay

_client = acmepay.Client("sk_test")


@dataclass(frozen=True)
class Charge:
    id: str
    amount_cents: int
    currency: str
    brand: str


def list_charges(month="2026-10"):
    return [Charge(c.id, c.amount, c.currency.upper(), c.payment_method.card.brand) for c in _client.charges.list(month)]
