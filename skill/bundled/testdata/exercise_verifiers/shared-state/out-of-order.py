import hashlib
import os
import shutil
import subprocess
import sys

sys.path.insert(0, ".")
fails = []
ORIGINAL = '''class Mirror:
    def __init__(self):
        self.rows = {}

    def status(self, order_id):
        row = self.rows.get(order_id)
        return row["status"] if row else None


def on_order_updated(mirror, api, event):
    """Handle {"order": id, "status": s, "version": n}, sent when an order changes."""
    mirror.rows[event["order"]] = {"status": event["status"], "version": event["version"]}
'''


def h(p):
    return hashlib.sha256(open(p, "rb").read().replace(b"\r\n", b"\n")).hexdigest()[:16]


try:
    if h("vendor/orders_api.py") != "740c2ed05486d870":
        fails.append("vendor/orders_api.py changed; it is the shared client and the fix has to work with it as it is")
except OSError:
    fails.append("vendor/orders_api.py is gone")


def ev(order, status, version):
    return {"order": order, "status": status, "version": version}


def S(status, version):
    return {"status": status, "version": version}


try:
    from mirror import Mirror, on_order_updated
    from vendor.orders_api import ApiDown, OrdersApi

    def deliver(m, api, *events):
        for e in events:
            try:
                on_order_updated(m, api, e)
            except ApiDown:
                pass

    def case(name, orders, events, order, want):
        api = OrdersApi(orders)
        m = Mirror()
        try:
            deliver(m, api, *events)
        except Exception as e:
            fails.append("%s: the handler raised %r" % (name, e))
            return None
        got = m.status(order)
        if got != want:
            fails.append("%s: the mirror shows %r, want %r" % (name, got, want))
        return m

    # events swapped in transit, repeated, or one dropped
    case("events in reverse", {"o1": S("shipped", 2)}, [ev("o1", "shipped", 2), ev("o1", "paid", 1)], "o1", "shipped")
    case("duplicates", {"o1": S("shipped", 2)}, [ev("o1", "shipped", 2)] * 3 + [ev("o1", "paid", 1)] * 2, "o1", "shipped")
    case("a dropped event", {"o1": S("delivered", 3)}, [ev("o1", "paid", 1), ev("o1", "shipped", 2)], "o1", "delivered")

    # a replica that is behind: the mirror never goes back
    api = OrdersApi({"o1": S("delivered", 3)})
    m = Mirror()
    deliver(m, api, ev("o1", "delivered", 3))
    api.orders["o1"] = S("shipped", 2)
    deliver(m, api, ev("o1", "shipped", 2), ev("o1", "shipped", 2))
    if m.status("o1") != "delivered":
        fails.append("a read from a replica that is behind took the mirror back to %r" % (m.status("o1"),))
    api.orders["o1"] = S("returned", 4)
    deliver(m, api, ev("o1", "paid", 1))
    if m.status("o1") != "returned":
        fails.append("a later change read after an old event left %r, want 'returned'" % (m.status("o1"),))

    # the service is down
    api = OrdersApi({"o1": S("delivered", 3)})
    m = Mirror()
    deliver(m, api, ev("o1", "delivered", 3))
    api.down = True
    deliver(m, api, ev("o1", "shipped", 2), ev("o1", "paid", 1))
    if m.status("o1") != "delivered":
        fails.append("while the orders service was down an event changed the mirror to %r; it must keep what it has" % (m.status("o1"),))
    api.down = False
    deliver(m, api, ev("o1", "shipped", 2))
    if m.status("o1") != "delivered":
        fails.append("after the service came back the mirror shows %r, want 'delivered'" % (m.status("o1"),))
except Exception as e:
    fails.append("on_order_updated could not be exercised: %r" % (e,))


def run(d):
    p = subprocess.run([sys.executable, "-m", "unittest", "discover", "-s", ".", "-p", "test*.py"],
                       cwd=d, capture_output=True, encoding="utf-8", errors="replace", timeout=60)
    return p.returncode, p.stdout + p.stderr


rc, out = run(".")
if rc != 0:
    fails.append("the tests fail on the result: " + out[-300:])
d = "_mut_original"
shutil.copytree(".", d, ignore=shutil.ignore_patterns("_mut_*", "__pycache__", ".git", ".flynn-ab"))
with open(os.path.join(d, "mirror.py"), "w", newline="\n", encoding="utf-8") as f:
    f.write(ORIGINAL)
rc, out = run(d)
shutil.rmtree(d, ignore_errors=True)
if rc == 0 or "AssertionError" not in out:
    fails.append("the tests do not fail with an assertion on the original handler; the fix ships with a test that fails without it")
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
