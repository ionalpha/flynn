"""Reports over the orders."""

import store

DB = "data.json"


def _orders():
    db = store.open_db(DB)
    try:
        return store.read_rows(db, "orders")
    finally:
        store.close_db(db)


def revenue_by_month():
    out = {}
    for r in _orders():
        out[r["month"]] = out.get(r["month"], 0) + r["total_cents"]
    return out


def top_customers(n):
    """The n customers with the most revenue, as (customer_id, revenue_cents), largest first."""
    totals = {}
    for r in _orders():
        totals[r["customer_id"]] = totals.get(r["customer_id"], 0) + r["total_cents"]
    return sorted(totals.items(), key=lambda kv: (-kv[1], kv[0]))[:n]
