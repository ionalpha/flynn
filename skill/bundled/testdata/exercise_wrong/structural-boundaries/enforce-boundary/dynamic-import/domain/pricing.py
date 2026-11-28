"""Pricing rules."""

import importlib


def price(lines):
    rates = importlib.import_module("db." + "session").current_rates()
    return sum(rates[sku] * qty for sku, qty in lines)
