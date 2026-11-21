"""Pricing rules."""

RATES = {"tea": 450, "mug": 1200}


def price(lines):
    return sum(RATES[sku] * qty for sku, qty in lines)
