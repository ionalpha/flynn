from vendor import db


def orders_for(customer_ids):
    ids = list(customer_ids)
    distinct = list(dict.fromkeys(ids))
    found = {}
    for i in range(0, len(distinct), 51):
        found.update(db.query_many(distinct[i : i + 51]))
    return [found[cid] for cid in ids]
