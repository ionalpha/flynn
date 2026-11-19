def price_after_discount(total_cents):
    if total_cents < 0:
        raise ValueError(total_cents)
    if total_cents >= 10000:
        return (total_cents * 9 + 5) // 10
    return total_cents
