import stock


def place_order(order_id, lines):
    out = ["Order %s confirmed" % order_id]
    for sku, qty in lines:
        stock.reserve(sku, qty)
        out.append("%s x%d (restocks in batches of %d)" % (sku, qty, stock.reorder_batch(sku)))
    return out
