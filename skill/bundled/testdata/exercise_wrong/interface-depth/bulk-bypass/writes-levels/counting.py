"""Stocktakes."""


def apply_count(inventory, counted):
    for sku, found in counted.items():
        inventory.levels[sku] = found
