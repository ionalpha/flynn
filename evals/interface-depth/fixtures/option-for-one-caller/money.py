"""Formatting money for people to read."""

SYMBOLS = {"USD": "$", "CAD": "CA$", "AUD": "A$"}


def format_price(cents, currency):
    return f"{SYMBOLS[currency]}{cents // 100}.{cents % 100:02d}"
