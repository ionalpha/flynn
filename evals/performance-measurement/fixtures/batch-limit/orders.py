from vendor import db


def orders_for(customer_ids):
    return [db.query_one(cid) for cid in customer_ids]
