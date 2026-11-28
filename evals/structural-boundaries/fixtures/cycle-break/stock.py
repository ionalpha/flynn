ON_HAND = {"SKU-LAMP": 10, "SKU-DESK": 4, "SKU-MUG": 8}
REORDER_LEVEL = {"SKU-LAMP": 5, "SKU-DESK": 2, "SKU-MUG": 6}


class OutOfStock(Exception):
    pass


def reserve(sku, qty):
    if ON_HAND.get(sku, 0) < qty:
        raise OutOfStock(sku)
    ON_HAND[sku] -= qty
