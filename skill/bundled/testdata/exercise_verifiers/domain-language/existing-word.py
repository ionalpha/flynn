import ast
import importlib
import os
import re
import sys

sys.path.insert(0, ".")
fails = []
ORIGINAL = {"find_customer", "orders_for_customer", "order_count"}


def words(name):
    parts = re.sub(r"([a-z0-9])([A-Z])", r"\1_\2", name).lower().split("_")
    return [p for p in parts if p]


for root, dirs, files in os.walk("."):
    dirs[:] = [d for d in dirs if d not in (".git", ".flynn-ab", "__pycache__")]
    for name in files:
        if "client" in words(name.rsplit(".", 1)[0]) or "clients" in words(name.rsplit(".", 1)[0]):
            fails.append("a file is named " + name)
        if not name.endswith(".py"):
            continue
        p = os.path.join(root, name)
        for node in ast.walk(ast.parse(open(p, encoding="utf-8").read())):
            ids = []
            if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef)):
                ids = [node.name]
            elif isinstance(node, ast.arg):
                ids = [node.arg]
            elif isinstance(node, ast.Name):
                ids = [node.id]
            elif isinstance(node, ast.Attribute):
                ids = [node.attr]
            elif isinstance(node, ast.keyword) and node.arg:
                ids = [node.arg]
            for i in ids:
                if {"client", "clients"} & set(words(i)):
                    fails.append(p + " names something " + i + "; this code calls them customers")
found = []
for mod in ("customers", "orders") + tuple(f[:-3] for f in os.listdir(".") if f.endswith(".py") and not f.startswith("test")):
    try:
        m = importlib.import_module(mod)
    except Exception:
        continue
    import customers
    import orders

    for n, f in vars(m).items():
        if not callable(f) or n in ORIGINAL or n.startswith("_") or getattr(f, "__module__", "") != mod:
            continue
        for arg in (lambda c: c, lambda c: customers.find_customer(c)):
            try:
                if [f(arg(c)) for c in ("c1", "c2", "c3")] != [3500, 1200, 0]:
                    continue
                orders.ORDERS.append({"id": "o9", "customer_id": "c3", "total_cents": 700})
                try:
                    if [f(arg(c)) for c in ("c1", "c2", "c3")] == [3500, 1200, 700]:
                        found.append(mod + "." + n)
                        break
                finally:
                    orders.ORDERS.pop()
            except Exception:
                pass
if not found:
    fails.append("no new function returns what a customer has spent, from the orders as they stand: c1 3500, c2 1200, c3 0, and c3 700 after an order of 700 is added")
fails = list(dict.fromkeys(fails))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
