from money import format_price


def cart_line(name, cents, currency):
    return f"{name}: {format_price(cents, currency)}"
