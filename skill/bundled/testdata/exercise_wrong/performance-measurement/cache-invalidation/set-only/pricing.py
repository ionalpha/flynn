import store

_cache = {}


def get_price(sku):
    if sku not in _cache:
        _cache[sku] = store.read(sku)
    return _cache[sku]


def set_price(sku, cents):
    store.write(sku, cents)
    _cache.pop(sku, None)


def delete_price(sku):
    store.remove(sku)


def basket_total(skus):
    return sum(get_price(s) for s in skus)
