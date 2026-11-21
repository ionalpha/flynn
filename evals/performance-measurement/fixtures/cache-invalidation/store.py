PRICES = {}
READS = 0


def read(sku):
    """The stored price of sku in cents. Raises KeyError for an unknown sku."""
    global READS
    READS += 1
    return PRICES[sku]


def write(sku, cents):
    PRICES[sku] = cents


def remove(sku):
    del PRICES[sku]
