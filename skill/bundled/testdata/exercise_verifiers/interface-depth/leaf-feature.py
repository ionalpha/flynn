import hashlib
import os
import sys

sys.path.insert(0, ".")
fails = []
KEEP = {"store.py": "ec39fde2db50b4a199fc0dda3b68eecd2e6aef1f5929e3acc5215e9214071b10", "exports.py": "9a4c173373dd3afef9caf5d5d2c9e51e84920cafcba8b65bcdbd0df959cf7b1c"}
for path, want in KEEP.items():
    try:
        got = hashlib.sha256(open(path, "rb").read().replace(b"\r\n", b"\n")).hexdigest()
    except OSError:
        got = None
    if got != want:
        fails.append(path + " changed; a report that reads orders has no reason to restructure it")
ORIGINAL = {"store.py", "exports.py", "reports.py"}
for name in os.listdir("."):
    if name.endswith(".py") and name not in ORIGINAL and not name.startswith("test"):
        fails.append("a new module " + name + " was added for one function")
    if os.path.isdir(name) and name not in ("__pycache__", ".git") and not name.startswith("_mut"):
        fails.append("a new directory " + name + " was added for one function")
try:
    import reports

    got = reports.top_customers(2)
    if [tuple(p) for p in got] != [("c1", 3500), ("c3", 900)]:
        fails.append("top_customers(2) is %r, want [('c1', 3500), ('c3', 900)]" % (got,))
    got = reports.top_customers(5)
    if [tuple(p) for p in got] != [("c1", 3500), ("c3", 900), ("c2", 400)]:
        fails.append("top_customers(5) is %r, want all three customers" % (got,))
    if reports.top_customers(0) != [] or [tuple(p) for p in reports.top_customers(1)] != [("c1", 3500)]:
        fails.append("top_customers(0) and top_customers(1) are %r and %r" % (reports.top_customers(0), reports.top_customers(1)))
    probe = "_probe_orders.json"
    with open(probe, "w", encoding="utf-8", newline=chr(10)) as f:
        f.write('{"orders": [{"id": "x1", "customer_id": "z1", "month": "2026-03", "total_cents": 50}, {"id": "x2", "customer_id": "z2", "month": "2026-03", "total_cents": 70}, {"id": "x3", "customer_id": "z1", "month": "2026-04", "total_cents": 40}]}')
    keep = reports.DB
    reports.DB = probe
    try:
        got = [tuple(p) for p in reports.top_customers(2)]
    finally:
        reports.DB = keep
        os.remove(probe)
    if got != [("z1", 90), ("z2", 70)]:
        fails.append("top_customers on other orders is %r, want [('z1', 90), ('z2', 70)]: it reads the orders it is given" % (got,))
    if reports.revenue_by_month() != {"2026-01": 2400, "2026-02": 2400}:
        fails.append("revenue_by_month changed: %r" % (reports.revenue_by_month(),))
except Exception as e:
    fails.append("reports could not be exercised: %r" % (e,))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
