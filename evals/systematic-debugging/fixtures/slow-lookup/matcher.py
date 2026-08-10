"""Match order lines to catalogue entries by SKU."""


def match(orders, catalogue):
    """Return one catalogue entry per order line, in order."""
    out = []
    for order in orders:
        for entry in catalogue:
            if entry["sku"] == order["sku"]:
                out.append(entry)
                break
        else:
            out.append(None)
    return out
