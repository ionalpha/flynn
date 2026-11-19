"""The monthly payments report."""

import acmepay

from payments import list_charges

_sdk = acmepay.Client("sk_test")


def _money(cents):
    return f"{cents // 100}.{cents % 100:02d}"


def monthly_lines(month="2026-10"):
    return [f"{c.id}  {_money(c.amount_cents)} {c.currency} {_sdk.charges.retrieve(c.id).payment_method.card.brand}" for c in list_charges(month)]
