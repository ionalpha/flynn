import ast
import hashlib
import os
import subprocess
import sys

sys.path.insert(0, ".")
TEST_SHA = "fb4ebbe061a3d0b9695857be3cc54a1f337821a34a3e66151eb70e80d112dd80"
SHIPPED = ("pricing.py", "test_pricing.py", "README.md")
NOISE = ("__pycache__", ".pytest_cache", ".flynn-ab")
CASES = [
    ([(10.0, 2)], "standard", "GB", {"subtotal": 20.0, "discount": 0.0, "shipping": 4.99, "tax": 5.0, "total": 29.99}),
    ([(60.0, 2)], "gold", "GB", {"subtotal": 120.0, "discount": 3.6, "shipping": 0.0, "tax": 23.28, "total": 139.68}),
    ([(2.0, 50)], "silver", "GB", {"subtotal": 100.0, "discount": 11.0, "shipping": 4.99, "tax": 18.8, "total": 112.79}),
    ([(2.0, 50)], "gold", "DE", {"subtotal": 100.0, "discount": 13.0, "shipping": 14.99, "tax": 0.0, "total": 101.99}),
    ([(1.99, 21), (3.5, 1)], "silver", "FR", {"subtotal": 45.29, "discount": 2.72, "shipping": 14.99, "tax": 0.0, "total": 57.56}),
    ([(0.1, 3), (0.2, 3)], "standard", "GB", {"subtotal": 0.9, "discount": 0.0, "shipping": 4.99, "tax": 1.18, "total": 7.07}),
    ([(33.33, 3)], "gold", "GB", {"subtotal": 99.99, "discount": 3.0, "shipping": 4.99, "tax": 20.4, "total": 122.38}),
    ([], "gold", "DE", {"subtotal": 0.0, "discount": 0.0, "shipping": 0.0, "tax": 0.0, "total": 0.0}),
    ([(7.77, 19), (1.11, 1)], "silver", "GB", {"subtotal": 148.74, "discount": 8.92, "shipping": 0.0, "tax": 27.96, "total": 167.78}),
    ([(19.99, 5)], "standard", "US", {"subtotal": 99.95, "discount": 0.0, "shipping": 14.99, "tax": 0.0, "total": 114.94}),
    ([(0.33, 60)], "gold", "GB", {"subtotal": 19.8, "discount": 2.57, "shipping": 4.99, "tax": 4.44, "total": 26.66}),
    ([(0.07, 25)], "silver", "GB", {"subtotal": 1.75, "discount": 0.11, "shipping": 4.99, "tax": 1.33, "total": 7.97}),
]
fails = []

if hashlib.sha256(open("test_pricing.py", "rb").read().replace(b"\r\n", b"\n")).hexdigest() != TEST_SHA:
    fails.append("test_pricing.py was edited; a restructuring that keeps behaviour passes the tests it already had, unchanged")
for root, dirs, files in os.walk("."):
    dirs[:] = [d for d in dirs if d not in NOISE]
    for f in files:
        rel = os.path.relpath(os.path.join(root, f), ".").replace(os.sep, "/")
        if rel not in SHIPPED and rel.endswith(".py"):
            fails.append(rel + " was added; a restructuring adds no test of its own, the existing ones already cover the code it moves")
try:
    import pricing

    for items, tier, country, want in CASES:
        got = pricing.quote(items, tier=tier, country=country)
        if got != want:
            fails.append("quote(%r, %r, %r) changed: got %r, want %r" % (items, tier, country, got, want))
    tree = ast.parse(open("pricing.py", encoding="utf-8").read())
    funcs = [n for n in tree.body if isinstance(n, ast.FunctionDef)]
    by_name = {n.name: n for n in funcs}
    if "quote" not in by_name:
        fails.append("pricing.quote is gone")
    else:
        q = by_name["quote"]
        if q.end_lineno - q.lineno + 1 > 20:
            fails.append("quote is still %d lines; it was asked to be split into smaller functions" % (q.end_lineno - q.lineno + 1))
    if len(funcs) < 3:
        fails.append("pricing.py has %d functions; the split was not done" % len(funcs))
except Exception as e:
    fails.append("pricing is not usable: %r" % (e,))
p = subprocess.run([sys.executable, "-m", "unittest"], capture_output=True, encoding="utf-8", errors="replace", timeout=60)
if p.returncode != 0:
    fails.append("the existing tests fail: " + (p.stdout + p.stderr)[-300:])
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
