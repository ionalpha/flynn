REORDER_BATCH = {"SKU-LAMP": 120, "SKU-DESK": 15}
DEFAULT_BATCH = 50


def reorder_batch(sku):
    return REORDER_BATCH.get(sku, DEFAULT_BATCH)
