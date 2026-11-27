"""Nightly copy of the catalog into the shop."""


def fetch_all(client, limit=3):
    products = []
    cursor = None
    while True:
        page = client.list_products(cursor=cursor, limit=limit)
        products.extend(page["items"])
        cursor = page["next"]
        if cursor is None:
            return products


def sync(client, shop):
    """Make shop (sku -> product) match the catalog."""
    products = {p["sku"]: p for p in fetch_all(client)}
    shop.update(products)
    for sku in list(shop):
        if sku not in products:
            del shop[sku]
