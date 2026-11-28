import stock


def transfer(sku, qty, site):
    stock.reserve(sku, qty)
    return "%d x %s sent to %s" % (qty, sku, site)
