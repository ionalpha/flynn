import store


def top_customers(n):
    db = store.open_db("data.json")
    try:
        rows = store.read_rows(db, "orders")
    finally:
        store.close_db(db)
    totals = {}
    for r in rows:
        totals[r["customer_id"]] = totals.get(r["customer_id"], 0) + r["total_cents"]
    return sorted(totals.items(), key=lambda kv: (-kv[1], kv[0]))[:n]
