"""Pricing a cart at checkout."""

from catalog import load_prices
from discount_codes import DiscountService

_discounts = DiscountService()


def checkout_total(cart, code=None):
    """The total in cents for a cart of (sku, quantity) pairs."""
    prices = load_prices()
    total = sum(prices[sku] * qty for sku, qty in cart)
    return _discounts.apply(total, code)
