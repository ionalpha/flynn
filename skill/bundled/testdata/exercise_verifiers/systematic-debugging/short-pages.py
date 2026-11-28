import sys
sys.path.insert(0, ".")
fails = []
try:
    from vendor.catalog import Catalog
    from sync import sync

    class Counting(Catalog):
        calls = 0

        def list_products(self, cursor=None, limit=100):
            self.calls += 1
            if self.calls > 5000:
                raise RuntimeError("more than 5000 page requests")
            return super().list_products(cursor=cursor, limit=limit)

    skus = ["s%d" % i for i in range(1500)]
    for hidden in (["s1"], skus[:400]):
        shop = {"s1499": {"sku": "s1499"}, "gone": {"sku": "gone"}}
        sync(Counting([{"sku": s} for s in skus], hidden), shop)
        want = set(skus) - set(hidden)
        if set(shop) != want:
            fails.append("with %d hidden: %d products missing, %d that should not be there" % (len(hidden), len(want - set(shop)), len(set(shop) - want)))
except Exception as e:
    fails.append("sync could not be exercised: %r" % e)
for x in fails:
    print(x, file=sys.stderr)
sys.exit(1 if fails else 0)
