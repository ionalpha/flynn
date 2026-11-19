"""Reserving stock for an order."""

import threading

_lock = threading.Lock()


class OutOfStock(Exception):
    pass


def reserve(store, sku, qty):
    with _lock:
        stock, _ = store.get(sku)
        if stock < qty:
            raise OutOfStock(sku)
        store.put(sku, stock - qty)
