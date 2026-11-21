"""Pricing a cart at checkout."""

from catalog import load_prices


def checkout_total(cart):
    """The total in cents for a cart of (sku, quantity) pairs."""
    prices = load_prices()
    return sum(prices[sku] * qty for sku, qty in cart)
