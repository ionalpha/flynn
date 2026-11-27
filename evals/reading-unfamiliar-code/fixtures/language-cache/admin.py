from app import CACHE
from catalog import PRODUCTS


def set_price(sku, cents):
    PRODUCTS[sku]["cents"] = cents
    CACHE.drop(sku)
