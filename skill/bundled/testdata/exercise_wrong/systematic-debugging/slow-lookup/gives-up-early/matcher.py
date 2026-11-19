"""Match order lines to catalogue entries by SKU."""


def match(orders, catalogue):
    out = []
    for order in orders:
        for entry in catalogue[:1000]:
            if entry["sku"] == order["sku"]:
                out.append(entry)
                break
        else:
            out.append(None)
    return out
