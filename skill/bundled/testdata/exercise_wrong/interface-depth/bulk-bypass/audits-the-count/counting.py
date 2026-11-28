"""Stocktakes."""


def apply_count(inventory, counted):
    for sku, found in counted.items():
        inventory.levels[sku] = found
        inventory.audit.append((sku, found, "stocktake"))
