"""Formatting money for people to read."""

SYMBOLS = {"USD": "$", "CAD": "CA$", "AUD": "A$"}


def format_amount(cents):
    """The amount alone, for places that state the currency elsewhere."""
    return f"{cents // 100}.{cents % 100:02d}"


def format_price(cents, currency):
    return SYMBOLS[currency] + format_amount(cents)
