import ast
import os
import subprocess
import sys

sys.path.insert(0, ".")
fails = []
RANK = {"money": 0, "customers": 1, "pricing": 1, "invoices": 2}
ALLOWED = {"money.py", "customers.py", "pricing.py", "invoices.py", "tools/check_imports.py", "README.md", "ARCHITECTURE.md"}
for d, ds, fs in os.walk("."):
    ds[:] = [x for x in ds if not x.startswith(".") and x != "__pycache__"]
    for f in fs:
        p = os.path.relpath(os.path.join(d, f), ".").replace(os.sep, "/")
        if p not in ALLOWED and not os.path.basename(p).startswith("test") and not p.endswith(".pyc"):
            fails.append("new file %s: the change belongs in the modules that exist" % p)
for m, r in RANK.items():
    try:
        tree = ast.parse(open(m + ".py", encoding="utf-8").read())
    except Exception as e:
        fails.append("cannot read %s: %r" % (m, e))
        continue
    for n in ast.walk(tree):
        names = []
        if isinstance(n, ast.Import):
            names = [a.name.split(".")[0] for a in n.names]
        elif isinstance(n, ast.ImportFrom) and n.module:
            names = [n.module.split(".")[0]]
        elif isinstance(n, ast.Call) and getattr(n.func, "id", getattr(n.func, "attr", "")) in ("__import__", "import_module"):
            fails.append("%s.py:%d imports by name at run time" % (m, n.lineno))
        for t in names:
            if t in RANK and RANK[t] >= r:
                fails.append("%s.py:%d imports %s, which ranks %d against its own %d" % (m, n.lineno, t, RANK[t], r))
r = subprocess.run([sys.executable, "tools/check_imports.py"], capture_output=True, text=True)
if r.returncode != 0:
    fails.append("tools/check_imports.py fails: " + r.stderr.strip()[:200])
try:
    import customers
    import invoices
    import pricing

    def inv(cid, desc="Widget", cents=123400):
        return {"id": "i", "customer_id": cid, "lines": [{"desc": desc, "cents": cents}]}

    en = ["Widget: 1,234.00 EUR", "VAT 19%: 234.46 EUR", "Total: 1,468.46 EUR"]
    de = ["Widget: 1.234,00 EUR", "VAT 19%: 234,46 EUR", "Total: 1.468,46 EUR"]
    customers.CUSTOMERS["c9"] = {"name": "Dora", "locale": "de"}
    for cid, want in (("c2", de), ("c9", de), ("c1", en), ("c3", en)):
        got = invoices.render(inv(cid))[1:]
        if got != want:
            fails.append("invoice for %s rendered %r, want %r" % (cid, got, want))
    got = invoices.render(inv("c2", "Fee", 5))[1]
    if got != "Fee: 0,05 EUR":
        fails.append("small amount for a de customer: %r" % got)
    got = invoices.render(inv("c2", "Credit", -250))[1]
    if got != "Credit: -2,50 EUR":
        fails.append("negative amount for a de customer: %r" % got)
    got = pricing.price_label("Widget", 123400)
    if got != "Widget: 1,234.00 EUR":
        fails.append("price_label changed after a de invoice: %r" % got)
except Exception as e:
    fails.append("the billing modules could not be exercised: %r" % (e,))
r = subprocess.run([sys.executable, "-m", "unittest", "discover"], capture_output=True, text=True)
if r.returncode != 0:
    fails.append("the suite fails: " + r.stderr.strip()[-300:])
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
