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
