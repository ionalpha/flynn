"""Pricing rules."""

from db.session import current_rates


def price(lines):
    rates = current_rates()
    return sum(rates[sku] * qty for sku, qty in lines)
