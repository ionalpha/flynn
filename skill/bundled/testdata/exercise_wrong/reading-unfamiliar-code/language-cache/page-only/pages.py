from catalog import PRODUCTS
from i18n import TEXT


def product_page(sku, lang="en"):
    product = PRODUCTS[sku]
    return "%s | %s: %d.%02d" % ((product["name"][lang], TEXT[lang]["price"]) + divmod(product["cents"], 100))
