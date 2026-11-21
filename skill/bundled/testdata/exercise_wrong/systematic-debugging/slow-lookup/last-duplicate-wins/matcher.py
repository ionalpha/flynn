"""Match order lines to catalogue entries by SKU."""


def match(orders, catalogue):
    index = {entry["sku"]: entry for entry in catalogue}
    return [index.get(order["sku"]) for order in orders]
