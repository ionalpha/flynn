from billing.money import parse_money


def invoice_total(lines):
    return sum(parse_money(x) for x in lines)
