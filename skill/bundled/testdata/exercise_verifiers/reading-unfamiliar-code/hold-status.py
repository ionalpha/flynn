import os
import shutil
import subprocess
import sys

sys.path.insert(0, ".")
sys.dont_write_bytecode = True
fails = []


def suite(d="."):
    p = subprocess.run([sys.executable, "-B", "-m", "unittest", "discover", "-s", ".", "-p", "test*.py"],
                       cwd=d, capture_output=True, encoding="utf-8", errors="replace", timeout=60)
    return p.returncode, p.stderr


rc, out = suite()
if rc != 0:
    fails.append("the project's tests fail: " + out[-300:])

try:
    import orders
    from nightly import expire
    from warehouse import pick_list

    def refused(fn, o):
        before = o.status
        try:
            fn(o)
        except ValueError:
            if o.status != before:
                fails.append("a refused %s changed the order" % fn.__name__)
            return
        fails.append("%s accepted a %s order" % (fn.__name__, before))

    new, paid = orders.Order("N", 1), orders.Order("P", 1)
    orders.pay(paid)
    for o in (new, paid):
        orders.hold(o)
        if o.status != "on_hold":
            fails.append("a held order has status %r" % o.status)
    for o in (new, paid):
        refused(orders.ship, o)
        refused(orders.cancel, o)
    if pick_list([new, paid]) != []:
        fails.append("the warehouse is told to pack held orders: %r" % (pick_list([new, paid]),))
    expire([new, paid], 100)
    if (new.status, paid.status) != ("on_hold", "on_hold"):
        fails.append("the nightly job changed held orders to %r" % ((new.status, paid.status),))
    new.status = paid.status = "on_hold"
    orders.release(new)
    orders.release(paid)
    if (new.status, paid.status) != ("new", "paid"):
        fails.append("release left %r, want ('new', 'paid')" % ((new.status, paid.status),))
    if pick_list([new, paid]) != ["P"]:
        fails.append("after release the pick list is %r" % (pick_list([new, paid]),))
    expire([new, paid], 100)
    if (new.status, paid.status) != ("cancelled", "paid"):
        fails.append("after release the nightly job left %r" % ((new.status, paid.status),))
    for status in ("shipped", "cancelled"):
        refused(orders.hold, orders.Order("X", 1, status))
except Exception as e:
    fails.append("holding an order is not usable: %r" % (e,))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
