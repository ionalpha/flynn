"""Parsing money off the payment provider's CSV export."""


def parse_amount(text):
    """Return the amount in whole cents.

    The provider writes amounts as decimal strings: "12.34", "0.07", "1200".
    """
    return int(float(text) * 100)
