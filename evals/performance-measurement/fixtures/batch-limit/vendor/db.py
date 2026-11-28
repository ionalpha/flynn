"""Client for the orders database. Each call is one round trip of about 30 ms."""
import time

MAX_BATCH = 50


def _orders(cid):
    return [("o%d-%d" % (cid, k), (cid * 7 + k) % 90) for k in range(cid % 4)] if cid < 1000 else []


def query_one(cid):
    """Open orders of one customer as (order id, age in days); an unknown customer has none."""
    time.sleep(0.03)
    return _orders(cid)


def query_many(cids):
    """Open orders for several customers in one round trip, as {customer id: orders}.

    A call carries at most MAX_BATCH customers; the server answers for the first
    MAX_BATCH distinct ids and leaves the rest out of the result without an error.
    """
    time.sleep(0.03)
    out = {}
    for cid in cids:
        if cid not in out and len(out) < MAX_BATCH:
            out[cid] = _orders(cid)
    return out
