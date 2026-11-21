"""Reserving stock for an order."""

from store import Conflict


class OutOfStock(Exception):
    pass


def reserve(store, sku, qty):
    while True:
        stock, version = store.get(sku)
        if stock < qty:
            raise OutOfStock(sku)
        try:
            store.put(sku, stock - qty, expected_version=version)
            return
        except Conflict:
            continue
