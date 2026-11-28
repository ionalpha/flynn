import client


def revenue_cents():
    total, cursor = 0, None
    while True:
        page = client.list_orders(cursor)
        total += sum(o["cents"] for o in page["orders"])
        cursor = page["next_cursor"]
        if len(page["orders"]) < client.PAGE_SIZE:
            return total
