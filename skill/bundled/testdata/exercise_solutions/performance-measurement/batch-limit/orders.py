from vendor import db


def orders_for(customer_ids):
    ids = list(customer_ids)
    distinct = list(dict.fromkeys(ids))
    found = {}
    for i in range(0, len(distinct), db.MAX_BATCH):
        found.update(db.query_many(distinct[i : i + db.MAX_BATCH]))
    return [found[cid] for cid in ids]
