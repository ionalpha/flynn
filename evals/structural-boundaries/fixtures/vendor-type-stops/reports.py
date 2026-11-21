"""The monthly payments report."""

from payments import list_charges


def _money(cents):
    return f"{cents // 100}.{cents % 100:02d}"


def monthly_lines(month="2026-10"):
    return [f"{c.id}  {_money(c.amount_cents)} {c.currency}" for c in list_charges(month)]
