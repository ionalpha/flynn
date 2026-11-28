import money

VAT_PERCENT = 19


def vat_cents(net_cents):
    return net_cents * VAT_PERCENT // 100


def price_label(desc, cents):
    return "%s: %s" % (desc, money.fmt(cents))
