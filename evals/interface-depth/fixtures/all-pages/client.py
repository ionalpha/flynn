PAGE_SIZE = 100

ORDERS = [{"id": "o%d" % i, "customer": "c%d" % (i % 3), "cents": 100 + i, "deleted": False} for i in range(5)]


def list_orders(cursor=None):
    """Return {"orders": [...], "next_cursor": ...}, one page at a time.

    The service cuts pages of PAGE_SIZE records and then drops the deleted ones, so a
    page may hold fewer than PAGE_SIZE orders, or none, while more pages follow. Only
    a next_cursor of None ends the list.
    """
    start = cursor or 0
    end = start + PAGE_SIZE
    page = [o for o in ORDERS[start:end] if not o["deleted"]]
    return {"orders": page, "next_cursor": end if end < len(ORDERS) else None}
