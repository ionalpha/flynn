from money import format_price


def invoice_total(amounts, currency):
    return f"Total due: {format_price(sum(amounts), currency)}"
