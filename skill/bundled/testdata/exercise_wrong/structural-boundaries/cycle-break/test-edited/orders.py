REORDER_BATCH = {"SKU-LAMP": 120, "SKU-DESK": 15}
DEFAULT_BATCH = 50


def reorder_batch(sku):
    return REORDER_BATCH.get(sku, DEFAULT_BATCH)


def place_order(order_id, lines):
    import stock

    out = ["Order %s confirmed" % order_id]
    for sku, qty in lines:
        stock.reserve(sku, qty)
        out.append("%s x%d (restocks in batches of %d)" % (sku, qty, reorder_batch(sku)))
    return out
