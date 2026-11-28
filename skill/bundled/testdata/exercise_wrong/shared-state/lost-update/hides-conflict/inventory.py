"""Reserving stock for an order."""

from store import Conflict

ATTEMPTS = 5


class OutOfStock(Exception):
    pass


def reserve(store, sku, qty):
    """Take qty units of sku.

    The put carries the version the get saw, so a worker that read before another
    worker's write is refused and decides again from the new stock.
    """
    for attempt in range(ATTEMPTS):
        stock, version = store.get(sku)
        if stock < qty:
            raise OutOfStock(sku)
        try:
            store.put(sku, stock - qty, expected_version=version)
            return
        except Conflict:
            if attempt == ATTEMPTS - 1:
                raise RuntimeError("could not reserve " + sku)
