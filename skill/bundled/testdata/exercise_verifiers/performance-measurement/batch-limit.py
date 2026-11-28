import hashlib
import random
import subprocess
import sys

sys.path.insert(0, ".")
fails = []


def h(p):
    return hashlib.sha256(open(p, "rb").read().replace(b"\r\n", b"\n")).hexdigest()[:16]


for name, want in (("bench.py", "9bbfb014e0a4a4c5"), ("vendor/db.py", "205f7caa739def61")):
    try:
        if h(name) != want:
            fails.append(name + " was edited")
    except OSError:
        fails.append(name + " is gone")
r = subprocess.run([sys.executable, "test_orders.py"], capture_output=True, text=True)
if r.returncode != 0 or "test_one_list_per_requested_customer_in_order" not in open("test_orders.py").read():
    fails.append("test_orders.py no longer passes as it was")

import vendor.db as db

db.time = type("T", (), {"sleep": staticmethod(lambda s: None)})

CALLS = [0]
_one, _many = db.query_one, db.query_many


def one(cid):
    CALLS[0] += 1
    return _one(cid)


def many(cids):
    CALLS[0] += 1
    return _many(cids)


db.query_one, db.query_many = one, many


def ref(ids):
    return [[("o%d-%d" % (c, k), (c * 7 + k) % 90) for k in range(c % 4)] if c < 1000 else [] for c in ids]


rnd = random.Random(2)
CASES = (
    ("the benchmark's thirty customers", list(range(30))),
    ("nobody", []),
    ("exactly one full batch", list(range(100, 150))),
    ("one over a full batch", list(range(200, 251))),
    ("a big night with repeats and strangers", [rnd.randrange(1400) for _ in range(430)]),
    ("a few thousand customers", list(range(300, 2300))),
)
try:
    import orders

    for label, ids in CASES:
        CALLS[0] = 0
        got = orders.orders_for(list(ids))
        if got != ref(ids):
            fails.append("%s: the orders differ from one lookup per customer" % label)
        distinct = len(set(ids))
        most = -(-distinct // db.MAX_BATCH)
        if CALLS[0] > most:
            fails.append("%s: %d round trips for %d distinct customers, %d are enough" % (label, CALLS[0], distinct, most))
except Exception as e:
    fails.append("orders.orders_for is not usable: %r" % (e,))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
