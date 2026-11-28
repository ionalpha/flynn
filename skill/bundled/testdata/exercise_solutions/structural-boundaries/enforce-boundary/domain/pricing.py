"""Pricing rules. They take the rates as an argument so they never reach for db/."""


def price(lines, rates):
    return sum(rates[sku] * qty for sku, qty in lines)
