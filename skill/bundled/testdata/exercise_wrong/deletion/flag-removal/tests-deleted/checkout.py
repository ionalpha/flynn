BULK_PERCENT_OFF = 10
BULK_QTY = 10


def total(cart):
    return sum(_line_cents(line) for line in cart)


def round_half_up(num, den):
    return (num * 2 + den) // (den * 2)


def _line_cents(line):
    off = BULK_PERCENT_OFF if line["qty"] >= BULK_QTY else 0
    return round_half_up(line["cents"] * line["qty"] * (100 - off), 100)
