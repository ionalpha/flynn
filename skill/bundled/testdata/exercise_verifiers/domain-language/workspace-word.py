import ast
import hashlib
import os
import re
import subprocess
import sys

sys.path.insert(0, ".")
fails = []
KEEP = {
    "api.py": "4ea6c9232b15e6dc",
    "invites.py": "0aab33f5243d4efc",
    "db.py": "425d7559eb3e3a14",
    "schema.sql": "d4d2f934632069a1",
    "strings.json": "209986fadd6d51f6",
}
ALLOWED = {"README.md", "GLOSSARY.md", "api.py", "invites.py", "billing.py", "db.py", "schema.sql", "strings.json"}
OLD = {"LEDGER", "charge_account", "account_plan", "db"}
BANNED = {"tenant", "tenants", "org", "orgs", "organisation", "organization", "team", "teams", "account", "accounts", "client", "clients", "company", "ws"}


def words(name):
    spaced = re.sub(r"([a-z0-9])([A-Z])", r"\1 \2", name)
    return [w.lower() for w in re.split(r"[^A-Za-z0-9]+", spaced) if w]


for d, ds, fs in os.walk("."):
    ds[:] = [x for x in ds if not x.startswith(".") and x != "__pycache__"]
    for f in fs:
        p = os.path.relpath(os.path.join(d, f), ".").replace(os.sep, "/")
        if p not in ALLOWED and not f.startswith("test") and not f.endswith(".pyc"):
            fails.append("new file %s: the change belongs in billing.py" % p)
for f, h in KEEP.items():
    try:
        b = open(f, "rb").read().replace(b"\r\n", b"\n")
        if hashlib.sha256(b).hexdigest()[:16] != h:
            fails.append("%s was edited; a rename of the existing code was not asked for" % f)
    except OSError:
        fails.append("%s is gone" % f)
try:
    tree = ast.parse(open("billing.py", encoding="utf-8").read())
    top = {n.name: n for n in tree.body if isinstance(n, ast.FunctionDef)}
    for name, arg in (("charge_account", "account_id"), ("account_plan", "account_id")):
        if name not in top or top[name].args.args[0].arg != arg:
            fails.append("billing.%s was renamed or reshaped; existing callers still use it" % name)
    if "seats_left" not in top:
        fails.append("billing.seats_left does not exist")
    else:
        first = top["seats_left"].args.args[0].arg
        if "workspace" not in words(first):
            fails.append("seats_left takes %r; the schema, the screens and the tests all say workspace" % first)
    mine = []
    for n in tree.body:
        if isinstance(n, ast.FunctionDef) and n.name not in ("charge_account", "account_plan"):
            mine.append(n)
        elif isinstance(n, ast.Assign) and not any(getattr(t, "id", "") in OLD for t in n.targets):
            mine.append(n)
    for node in mine:
        for n in ast.walk(node):
            ident = None
            if isinstance(n, ast.FunctionDef):
                ident = n.name
            elif isinstance(n, ast.arg):
                ident = n.arg
            elif isinstance(n, ast.Name) and isinstance(n.ctx, ast.Store):
                ident = n.id
            if ident and set(words(ident)) & BANNED:
                fails.append("the new code names %r; this project's word is workspace" % ident)
except Exception as e:
    fails.append("cannot read billing.py: %r" % (e,))
try:
    import billing
    import db

    def left(w):
        return billing.seats_left(w)

    for w, want in (("w1", 2), ("w2", 0)):
        got = left(w)
        if got != want:
            fails.append("seats_left(%r) gave %r, want %d" % (w, got, want))
    db.CONN.execute("INSERT INTO members VALUES ('m9', 'w1', 'new@example.test', 1)")
    if left("w1") != 1:
        fails.append("an added active member did not use a seat")
    db.CONN.execute("INSERT INTO members VALUES ('m10', 'w1', 'gone@example.test', 0)")
    if left("w1") != 1:
        fails.append("an inactive member used a seat")
    db.CONN.execute("UPDATE workspaces SET seat_limit = 9 WHERE workspace_id = 'w1'")
    if left("w1") != 5:
        fails.append("seats_left ignores the seat limit in the workspaces table")
    if left("w2") != 0:
        fails.append("another workspace was affected")
except Exception as e:
    fails.append("billing could not be exercised: %r" % (e,))
r = subprocess.run([sys.executable, "-m", "unittest", "discover"], capture_output=True, text=True)
if r.returncode != 0:
    fails.append("the suite fails: " + r.stderr.strip()[-300:])
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
