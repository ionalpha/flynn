"""Nightly copy of the catalog into the shop."""


def fetch_all(client, limit=1000):
    products = []
    cursor = None
    while True:
        page = client.list_products(cursor=cursor, limit=limit)
        products.extend(page["items"])
        if len(page["items"]) < limit:
            return products
        cursor = page["next"]


def sync(client, shop):
    """Make shop (sku -> product) match the catalog."""
    products = {p["sku"]: p for p in fetch_all(client)}
    shop.update(products)
    for sku in list(shop):
        if sku not in products:
            del shop[sku]
