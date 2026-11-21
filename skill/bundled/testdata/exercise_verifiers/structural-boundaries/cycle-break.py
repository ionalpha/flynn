import ast
import os
import shutil
import subprocess
import sys
import tempfile

sys.path.insert(0, ".")
fails = []
MODS = ["purchasing", "stock", "orders", "warehouse"]
ALLOWED = {m + ".py" for m in MODS} | {"README.md", "ARCHITECTURE.md"}
graph = {}
counts = {120: 0, 15: 0}
for d, ds, fs in os.walk("."):
    ds[:] = [x for x in ds if not x.startswith(".") and x != "__pycache__"]
    for f in fs:
        p = os.path.relpath(os.path.join(d, f), ".").replace(os.sep, "/")
        if p not in ALLOWED and not f.startswith("test") and not f.endswith(".pyc"):
            fails.append("new file %s: the shared code belongs in the module that is more depended on" % p)
for m in MODS:
    graph[m] = set()
    for n in ast.walk(ast.parse(open(m + ".py", encoding="utf-8").read())):
        if isinstance(n, ast.Import):
            graph[m].update(a.name.split(".")[0] for a in n.names)
        elif isinstance(n, ast.ImportFrom) and n.module:
            graph[m].add(n.module.split(".")[0])
        elif isinstance(n, ast.Constant) and n.value in counts and type(n.value) is int:
            counts[n.value] += 1
        elif isinstance(n, ast.Call) and getattr(n.func, "id", getattr(n.func, "attr", "")) in ("__import__", "import_module"):
            fails.append("%s.py imports by name at run time" % m)
seen = {}


def visit(m, path):
    if m not in graph or seen.get(m) == 2:
        return
    if seen.get(m) == 1:
        fails.append("import cycle: " + " -> ".join(path + [m]))
        return
    seen[m] = 1
    for dep in sorted(graph[m]):
        visit(dep, path + [m])
    seen[m] = 2


for m in MODS:
    visit(m, [])
for v, c in counts.items():
    if c != 1:
        fails.append("the batch size %d is written %d times, want 1" % (v, c))
try:
    import orders
    import purchasing
    import stock

    def reset():
        stock.ON_HAND.update({"SKU-LAMP": 10, "SKU-DESK": 4, "SKU-MUG": 8})
        purchasing.REQUESTS.clear()

    reset()
    orders.place_order("a", [("SKU-LAMP", 3)])
    orders.place_order("b", [("SKU-LAMP", 3)])
    orders.place_order("c", [("SKU-LAMP", 1), ("SKU-DESK", 3), ("SKU-MUG", 3)])
    want = [("SKU-LAMP", 120), ("SKU-DESK", 15), ("SKU-MUG", 50)]
    if sorted(purchasing.REQUESTS) != sorted(want):
        fails.append("purchase requests were %r, want %r" % (purchasing.REQUESTS, want))
    out = orders.place_order("d", [("SKU-LAMP", 1)])
    if out[-1] != "SKU-LAMP x1 (restocks in batches of 120)":
        fails.append("the order confirmation changed: %r" % (out,))
except Exception as e:
    fails.append("the stock modules could not be exercised: %r" % (e,))
code = (
    "import warehouse, purchasing, stock\n"
    "stock.ON_HAND['SKU-LAMP'] = 6\n"
    "warehouse.transfer('SKU-LAMP', 2, 'east')\n"
    "print(purchasing.REQUESTS)\n"
)
r = subprocess.run([sys.executable, "-c", code], capture_output=True, text=True)
if r.stdout.strip() != "[('SKU-LAMP', 120)]":
    fails.append("a transfer that never imports orders raised %r (%s)" % (r.stdout.strip(), r.stderr.strip()[-120:]))
r = subprocess.run([sys.executable, "-m", "unittest", "discover"], capture_output=True, text=True)
if r.returncode != 0:
    fails.append("the suite fails: " + r.stderr.strip()[-300:])
tmp = tempfile.mkdtemp()
try:
    work = os.path.join(tmp, "w")
    tabs = os.path.abspath(tmp) + os.sep
    skip = lambda d, ns: [n for n in ns if n.startswith(".") or n == "__pycache__" or tabs.startswith(os.path.abspath(os.path.join(d, n)) + os.sep)]
    shutil.copytree(".", work, ignore=skip)
    with open(os.path.join(work, "stock.py"), "a", encoding="utf-8") as fh:
        fh.write("\nimport orders\n")
    r = subprocess.run([sys.executable, "-m", "unittest", "discover"], capture_output=True, text=True, cwd=work)
    if r.returncode == 0:
        fails.append("the cycle test no longer fails when stock imports orders")
finally:
    shutil.rmtree(tmp, ignore_errors=True)
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
