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
    if reports.revenue_by_month() != {"2026-01": 2400, "2026-02": 2400}:
        fails.append("revenue_by_month changed: %r" % (reports.revenue_by_month(),))
except Exception as e:
    fails.append("reports could not be exercised: %r" % (e,))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
