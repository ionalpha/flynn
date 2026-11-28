from billing.money import parse_money


def refund_cents(text, fee):
    return max(parse_money(text) - fee, 0)
