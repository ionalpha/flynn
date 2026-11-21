"""Parsing money off the payment provider's CSV export."""

from decimal import ROUND_HALF_UP, Decimal


def parse_amount(text):
    """Return the amount in whole cents.

    The provider writes amounts as decimal strings: "12.34", "0.07", "1200".
    Decimal keeps them exact; float(text) * 100 lands just under some whole cents.
    """
    return int((Decimal(text) * 100).quantize(Decimal(1), rounding=ROUND_HALF_UP))
