from vendor import db


def orders_for(customer_ids):
    ids = list(customer_ids)
    found = db.query_many(ids) if ids else {}
    return [found.get(cid, []) for cid in ids]
