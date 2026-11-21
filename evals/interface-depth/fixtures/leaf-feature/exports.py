"""The accounting export."""

import store

DB = "data.json"


def export_lines():
    db = store.open_db(DB)
    try:
        rows = store.read_rows(db, "orders")
    finally:
        store.close_db(db)
    return [f"{r['id']},{r['customer_id']},{r['total_cents']}" for r in rows]
