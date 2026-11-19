import store


def get_price(sku):
    return store.read(sku)


def set_price(sku, cents):
    store.write(sku, cents)


def delete_price(sku):
    store.remove(sku)


def basket_total(skus):
    return sum(get_price(s) for s in skus)
