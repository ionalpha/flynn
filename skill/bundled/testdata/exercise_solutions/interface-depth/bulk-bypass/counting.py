"""Stocktakes."""


def apply_count(inventory, counted):
    for sku, found in counted.items():
        delta = found - inventory.level(sku)
        if delta:
            inventory.adjust(sku, delta, "stocktake")
