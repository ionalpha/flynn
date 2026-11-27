import ast
import os
import subprocess
import sys

sys.path.insert(0, ".")
sys.dont_write_bytecode = True
fails = []


def files(ext):
    for root, dirs, names in os.walk("."):
        dirs[:] = [d for d in dirs if not d.startswith(".") and d != "__pycache__"]
        for n in names:
            if n.endswith(ext) and not n.startswith("."):
                yield os.path.join(root, n)[2:].replace(os.sep, "/")


def read(p):
    with open(p, encoding="utf-8", errors="replace") as f:
        return f.read()

try:

    for path in files(".py"):
        if not path.startswith("domain/"):
            continue
        for n in ast.walk(ast.parse(read(path))):
            names = []
            if isinstance(n, ast.Import):
                names = [a.name for a in n.names]
            elif isinstance(n, ast.ImportFrom):
                names = [n.module or ""]
            elif isinstance(n, ast.Constant) and isinstance(n.value, str):
                names = [n.value]
            if any(x == "adapters" or x.startswith("adapters.") for x in names):
                fails.append("%s reaches into adapters/, which ARCHITECTURE.md forbids" % path)
    import bulk_import
    import web
    from adapters import mail, stock
    from domain import orders

    web.post_order({"id": "w1", "email": "w@example.com", "cents": "700"})
    bulk_import.run([("b1", "b@example.com", 300)])
    to = sorted(m[0] for m in mail.SENT)
    if to != ["b@example.com", "w@example.com"]:
        fails.append("one web order and one imported order sent mail to %r; each customer should get one" % (to,))
    del mail.SENT[:]
    try:
        orders.place_order({"id": "x", "email": "x@example.com", "cents": 0})
        fails.append("an order for nothing was accepted")
    except ValueError:
        pass
    if mail.SENT:
        fails.append("a refused order was confirmed by mail")
    orders.cancel_order("w1")
    if "w1" not in stock.RELEASED:
        fails.append("cancelling no longer releases stock")
except Exception as e:
    fails.append("the code could not be exercised: %r" % (e,))
r = subprocess.run([sys.executable, "-B", "-m", "unittest", "discover"], capture_output=True, text=True)
if r.returncode != 0:
    fails.append("the suite fails: " + r.stderr.strip()[-300:])
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
