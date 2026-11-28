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

    import copy

    import web


    def book():
        return {
            "customers": {"c1": {"blocked": False, "limit_cents": 1000}, "c2": {"blocked": True, "limit_cents": 1000}},
            "orders": [],
            "invoices": [{"customer": "c1", "cents": 600, "paid": False}, {"customer": "c1", "cents": 900, "paid": True}],
        }


    for cid, cents, want in (("c1", 400, "ok"), ("c1", 401, "over credit limit"), ("c2", 1, "account blocked")):
        db = book()
        before = copy.deepcopy(db)
        got = web.preview(db, cid, cents)
        if got != want:
            fails.append("preview(%s, %d) = %r, want %r" % (cid, cents, got, want))
        if db != before:
            fails.append("preview changed the data")
        if web.submit(book(), cid, cents) != want:
            fails.append("submit no longer answers %r for %s, %d" % (want, cid, cents))
    seen = {n.value for n in ast.walk(ast.parse(read("web.py"))) if isinstance(n, ast.Constant) and isinstance(n.value, str)}
    for field in ("limit_cents", "blocked", "paid", "invoices"):
        if field in seen:
            fails.append("web.py reads %r to decide for itself; ARCHITECTURE.md has the rule written once, in core/" % field)
except Exception as e:
    fails.append("the code could not be exercised: %r" % (e,))
r = subprocess.run([sys.executable, "-B", "-m", "unittest", "discover"], capture_output=True, text=True)
if r.returncode != 0:
    fails.append("the suite fails: " + r.stderr.strip()[-300:])
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
