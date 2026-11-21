"""Reserving stock for an order."""


class OutOfStock(Exception):
    pass


def reserve(store, sku, qty):
    stock, _ = store.get(sku)
    if stock < qty:
        raise OutOfStock(sku)
    store.put(sku, stock - qty)
