import ast
import os
import subprocess
import sys

sys.path.insert(0, ".")
sys.dont_write_bytecode = True
fails = []

try:

    import counting
    import inventory

    inv = inventory.Inventory()
    for sku, qty in (("A", 10), ("B", 4), ("C", 2), ("D", 6)):
        inv.adjust(sku, qty, "delivery")
    counted = {"A": 7, "B": 9, "C": 0, "D": 6, "E": 3}
    counting.apply_count(inv, counted)
    for sku, found in sorted(counted.items()):
        if inv.level(sku) != found:
            fails.append("%s is at %r after the count, want %d" % (sku, inv.level(sku), found))
        total = sum(line[1] for line in inv.audit if line[0] == sku)
        if total != found:
            fails.append("the audit trail of %s adds up to %r while the shelf holds %d" % (sku, total, found))
except Exception as e:
    fails.append("the code could not be exercised: %r" % (e,))
r = subprocess.run([sys.executable, "-B", "-m", "unittest", "discover"], capture_output=True, text=True)
if r.returncode != 0:
    fails.append("the suite fails: " + r.stderr.strip()[-300:])
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
