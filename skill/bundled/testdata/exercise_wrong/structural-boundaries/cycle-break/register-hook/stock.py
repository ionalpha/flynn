import purchasing

ON_HAND = {"SKU-LAMP": 10, "SKU-DESK": 4, "SKU-MUG": 8}
REORDER_LEVEL = {"SKU-LAMP": 5, "SKU-DESK": 2, "SKU-MUG": 6}
BATCH_LOOKUP = None


class OutOfStock(Exception):
    pass


def set_batch_lookup(fn):
    global BATCH_LOOKUP
    BATCH_LOOKUP = fn


def reserve(sku, qty):
    if ON_HAND.get(sku, 0) < qty:
        raise OutOfStock(sku)
    before = ON_HAND[sku]
    ON_HAND[sku] = before - qty
    level = REORDER_LEVEL.get(sku)
    if level is not None and before > level >= ON_HAND[sku] and BATCH_LOOKUP is not None:
        purchasing.request(sku, BATCH_LOOKUP(sku))
