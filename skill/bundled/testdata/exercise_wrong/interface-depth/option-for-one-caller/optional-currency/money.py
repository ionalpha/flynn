"""Formatting money for people to read."""

SYMBOLS = {"USD": "$", "CAD": "CA$", "AUD": "A$"}


def format_price(cents, currency=None):
    amount = f"{cents // 100}.{cents % 100:02d}"
    return SYMBOLS[currency] + amount if currency else amount
