from catalog import PRODUCTS
from i18n import TEXT


def product_page(sku):
    product = PRODUCTS[sku]
    return "%s | %s: %d.%02d" % ((product["name"]["en"], TEXT["en"]["price"]) + divmod(product["cents"], 100))
