"""Pricing a cart at checkout."""

import json
import os

from catalog import load_prices

HERE = os.path.dirname(os.path.abspath(__file__))


def _percent_off(code):
    with open(os.path.join(HERE, "codes.json"), encoding="utf-8") as f:
        codes = json.load(f)
    if code not in codes:
        raise ValueError(f"unknown discount code {code!r}")
    return codes[code]


def checkout_total(cart, code=None):
    """The total in cents for a cart of (sku, quantity) pairs, less the code's percent off."""
    prices = load_prices()
    total = sum(prices[sku] * qty for sku, qty in cart)
    if code is None:
        return total
    return total * (100 - _percent_off(code)) // 100
