"""Money arithmetic for the ledger."""
from decimal import ROUND_HALF_UP, Decimal

CENT = Decimal("0.01")


def round_money(amount):
    """Round to whole cents, halves away from zero."""
    return amount.quantize(CENT, rounding=ROUND_HALF_UP)


def total(amounts):
    """Sum of the amounts, each rounded to the cent first."""
    return sum((round_money(a) for a in amounts), Decimal("0.00"))
