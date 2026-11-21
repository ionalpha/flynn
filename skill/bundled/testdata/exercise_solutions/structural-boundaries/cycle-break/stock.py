import purchasing

ON_HAND = {"SKU-LAMP": 10, "SKU-DESK": 4, "SKU-MUG": 8}
REORDER_LEVEL = {"SKU-LAMP": 5, "SKU-DESK": 2, "SKU-MUG": 6}
REORDER_BATCH = {"SKU-LAMP": 120, "SKU-DESK": 15}
DEFAULT_BATCH = 50


class OutOfStock(Exception):
    pass


def reorder_batch(sku):
    return REORDER_BATCH.get(sku, DEFAULT_BATCH)


def reserve(sku, qty):
    if ON_HAND.get(sku, 0) < qty:
        raise OutOfStock(sku)
    before = ON_HAND[sku]
    ON_HAND[sku] = before - qty
    level = REORDER_LEVEL.get(sku)
    if level is not None and before > level >= ON_HAND[sku]:
        purchasing.request(sku, reorder_batch(sku))
