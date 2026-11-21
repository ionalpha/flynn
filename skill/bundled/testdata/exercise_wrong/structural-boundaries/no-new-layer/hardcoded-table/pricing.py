"""Pricing a cart at checkout."""

from catalog import load_prices

CODES = {"SPRING10": 10, "STAFF25": 25}


def checkout_total(cart, code=None):
    """The total in cents for a cart of (sku, quantity) pairs, less the code's percent off."""
    prices = load_prices()
    total = sum(prices[sku] * qty for sku, qty in cart)
    if code is None:
        return total
    if code not in CODES:
        raise ValueError(f"unknown discount code {code!r}")
    return total * (100 - CODES[code]) // 100
