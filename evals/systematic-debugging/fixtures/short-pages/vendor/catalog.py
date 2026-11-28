"""Client for the product catalog. Vendored and shared with other services."""


class Catalog:
    def __init__(self, products, hidden=()):
        self._products = list(products)
        self._hidden = set(hidden)

    def list_products(self, cursor=None, limit=100):
        """Return {"items": [...], "next": cursor or None}.

        Withdrawn and region-locked products are removed after a page is cut, so a
        page can hold fewer than limit items, or none, while more pages follow.
        The listing ends only when next is None. limit is capped at 100.
        """
        start = int(cursor or 0)
        end = start + min(limit, 100)
        items = [p for p in self._products[start:end] if p["sku"] not in self._hidden]
        return {"items": items, "next": str(end) if end < len(self._products) else None}
