"""Match order lines to catalogue entries by SKU."""


def match(orders, catalogue):
    """Return one catalogue entry per order line, in order.

    The first catalogue entry for a SKU wins, as it did when this was a linear scan.
    """
    index = {}
    for entry in catalogue:
        index.setdefault(entry["sku"], entry)
    return [index.get(order["sku"]) for order in orders]
