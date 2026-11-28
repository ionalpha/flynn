import os, shutil, sys


def scratch():
    d = os.path.abspath("verify-tmp")
    shutil.rmtree(d, ignore_errors=True)
    os.makedirs(d)
    return d


sys.path.insert(0, ".")
fails = []
try:
    from report import totals_by_region
    got = totals_by_region("stores.csv", "sales.csv")
    want = {"north": 100, "south": 250, "west": 40, "east": 5}
    if got != want:
        fails.append("the shipped data totals %r, want %r" % (got, want))
    d = scratch()
    try:
        nl = chr(10)
        stores, sales = os.path.join(d, "stores.csv"), os.path.join(d, "sales.csv")
        with open(stores, "w") as f:
            f.write(nl.join(["store_id,region", "5,a", "05,b", "005,c", "0009,d", "40,e"]) + nl)
        with open(sales, "w") as f:
            f.write(nl.join(["store_id,cents", "5,1", "05,2", "005,4", "0009,8", "40,16", "005,32"]) + nl)
        got = totals_by_region(stores, sales)
        want = {"a": 1, "b": 2, "c": 36, "d": 8, "e": 16}
        if got != want:
            fails.append("ids that differ by leading zeros total %r, want %r" % (got, want))
    finally:
        shutil.rmtree(d, ignore_errors=True)
except Exception as e:
    fails.append("the report could not be exercised: %r" % e)
for x in fails:
    print(x, file=sys.stderr)
sys.exit(1 if fails else 0)
