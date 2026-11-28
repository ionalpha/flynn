import hashlib
import sys

sys.path.insert(0, ".")


def h(p):
    return hashlib.sha256(open(p, "rb").read().replace(b"\r\n", b"\n")).hexdigest()


fails = []
for name, want in (("store.py", "8e130e8669c5d82b3d4fc9b9f26448ae89a5b708b9cc26bc5dd9e33f8e60b7a4"),):
    if h(name) != want:
        fails.append(name + " was edited")

import pricing
import store

try:
    store.PRICES.clear()
    for sku, cents in (("a", 100), ("b", 250), ("c", 5)):
        pricing.set_price(sku, cents)
    store.READS = 0
    total = pricing.basket_total(["a", "a", "b", "a", "c", "b"])
    if total != 100 * 3 + 250 * 2 + 5:
        fails.append("basket_total is %r, want 805" % (total,))
    if store.READS > 3:
        fails.append("six items over three skus read the store %d times, want at most 3" % store.READS)

    pricing.set_price("a", 120)
    if pricing.get_price("a") != 120:
        fails.append("get_price after set_price is %r, want 120" % (pricing.get_price("a"),))
    if pricing.basket_total(["a", "b"]) != 370:
        fails.append("basket_total after set_price is %r, want 370" % (pricing.basket_total(["a", "b"]),))

    pricing.delete_price("b")
    try:
        got = pricing.get_price("b")
        fails.append("get_price of a deleted sku returned %r instead of raising KeyError" % (got,))
    except KeyError:
        pass
    pricing.set_price("b", 7)
    if pricing.get_price("b") != 7:
        fails.append("get_price after re-adding a deleted sku is %r, want 7" % (pricing.get_price("b"),))
    try:
        pricing.get_price("never-stored")
        fails.append("an unknown sku did not raise KeyError")
    except KeyError:
        pass
    store.READS = 0
    for _ in range(5):
        pricing.get_price("c")
    if store.READS > 1:
        fails.append("five lookups of one sku read the store %d times" % store.READS)
except Exception as e:
    fails.append("pricing is not usable: %r" % (e,))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
