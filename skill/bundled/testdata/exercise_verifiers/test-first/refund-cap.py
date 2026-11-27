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
    from refunds import Order, refund, refund_item, refunded

    def refused(o, fn, arg, why):
        before = (refunded(o), dict(o.items))
        try:
            fn(o, arg)
        except ValueError:
            if (refunded(o), dict(o.items)) != before:
                fails.append("a refused refund changed the order (%s)" % why)
            return
        fails.append("accepted: " + why)

    o = Order(1000, {"mug": 400, "pot": 600})
    refused(o, refund, 1001, "one refund above the amount paid")
    refund(o, 700)
    refused(o, refund, 301, "two goodwill refunds adding up to more than was paid")
    refused(o, refund_item, "mug", "a returned item after a goodwill refund, 1100 back on 1000 paid")
    refund(o, 300)
    if refunded(o) != 1000:
        fails.append("the full amount could not be refunded")
    o = Order(1000, {"mug": 400, "pot": 600})
    refund_item(o, "mug")
    refused(o, refund, 601, "a goodwill refund after a return, more than was paid in total")
    refund_item(o, "pot")
    if refunded(o) != 1000:
        fails.append("returning every item did not refund the order")
except Exception as e:
    fails.append("refunds is not usable: %r" % (e,))
if not fails:
    shutil.copytree(".", "_orig", ignore=shutil.ignore_patterns("_orig", "__pycache__", ".git", ".flynn-ab"))
    with open(os.path.join("_orig", "refunds.py"), "w", newline="\n", encoding="utf-8") as f:
        f.write('class Order:\n    def __init__(self, paid_cents, items):\n        self.paid_cents = paid_cents\n        self.items = dict(items)  # sku -> price of that item before any order discount\n        self.refunds = []\n\n\ndef refunded(order):\n    return sum(order.refunds)\n\n\ndef refund(order, cents):\n    if cents <= 0:\n        raise ValueError("a refund must be positive")\n    order.refunds.append(cents)\n\n\ndef refund_item(order, sku):\n    """Refund one returned item at its price."""\n    order.refunds.append(order.items.pop(sku))\n')
    if suite("_orig")[0] == 0:
        fails.append("the tests still pass on the code as it was, which over-refunds")
    shutil.rmtree("_orig")
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
