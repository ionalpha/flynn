"""Tiered discount on an order subtotal, in whole cents."""

TIERS = ((50_000, 20), (20_000, 10), (0, 0))


def discount_percent(subtotal_cents):
    for threshold, percent in TIERS:
        if subtotal_cents >= threshold:
            return percent
    return 0


def total_cents(subtotal_cents):
    percent = discount_percent(subtotal_cents)
    return subtotal_cents - (subtotal_cents * percent) // 100
