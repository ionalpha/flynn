"""Tiered discount on an order subtotal, in whole cents."""


def discount_percent(subtotal_cents):
    if subtotal_cents >= 50_000:
        return 20
    if subtotal_cents >= 20_000:
        return 10
    return 0


def total_cents(subtotal_cents):
    return subtotal_cents - (subtotal_cents * discount_percent(subtotal_cents)) // 100
