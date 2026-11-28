import ast
import contextlib
import hashlib
import importlib
import io
import os
import re
import subprocess
import sys

sys.path.insert(0, ".")
SHIPPED = {
    "README.md": "1a1ee29081542aa36bc6e93df30a5fbad8920c736dae67f064ebddb0a7c0f021",
    "client.py": "0c3a4c651e789d08d92ae75366476e83409fe5831ec40c30af1ee1b47cf62391",
    "cache.py": "993f6e7d218169611873270f83b3ed5ed9a95533a2dfd8fdcb0253f2c5cef46a",
    "test_app.py": "5db0ae65fb3d9f5fdde481d6707948d1308cfce4d3943f4033d4035cdab870be",
    "app/__init__.py": "3386626e6f12ee5c467e46be93231a90395da034cf65ff9d615372e8c7c8541c",
}
# What each module's old call asked for: True, False, "flag" (the second argument of run) or None.
ASKS = {
    "admin": "flag", "api": "flag", "audit": "flag", "billing": True, "cli": None, "digest": True,
    "export": True, "hooks": "flag", "invoices": "flag", "jobs": "flag", "mailer": "flag",
    "notes": "flag", "orders": "flag", "profile": False, "reports": "flag", "search": False,
    "sessions": "flag", "sync": "flag", "teams": "flag", "webhooks": "flag",
}
NOISE = ("__pycache__", ".pytest_cache", ".flynn-ab")


def h(p):
    return hashlib.sha256(open(p, "rb").read().replace(b"\r\n", b"\n")).hexdigest()


fails = []
present = set()
for root, dirs, files in os.walk("."):
    dirs[:] = [d for d in dirs if d not in NOISE]
    for f in files:
        present.add(os.path.relpath(os.path.join(root, f), ".").replace(os.sep, "/"))
wanted = set(SHIPPED) | {"app/%s.py" % m for m in ASKS}
for rel in sorted(present - wanted):
    fails.append(rel + " was left behind; only the project's own files stay")
for rel in sorted(wanted - present):
    fails.append(rel + " was removed")
for rel, digest in SHIPPED.items():
    if rel in present and h(rel) != digest:
        fails.append(rel + " was edited; the callers are what moves, not the client or the cache")

for m in ASKS:
    if "app/%s.py" % m not in present:
        continue
    for n in ast.walk(ast.parse(open("app/%s.py" % m, encoding="utf-8").read())):
        old = (
            (isinstance(n, ast.Name) and n.id == "fetch_user")
            or (isinstance(n, ast.alias) and n.name == "fetch_user")
            or (isinstance(n, ast.Constant) and n.value == "fetch_user")
            or (isinstance(n, ast.keyword) and n.arg == "deleted_ok")
        )
        if isinstance(n, ast.Attribute) and n.attr == "fetch_user":
            v = n.value
            old = not ((isinstance(v, ast.Name) and v.id == "cache") or (isinstance(v, ast.Call) and getattr(v.func, "id", "") == "Cache"))
        if old:
            fails.append("app/%s.py line %s still uses the removed call" % (m, getattr(n, "lineno", "?")))
            break

calls, cached = [], []
if not fails:
    try:
        import cache
        import client

        real, real_cached = client.get_user, cache.Cache.fetch_user

        def recorder(user_id, *, include_deleted=False):
            calls.append((user_id, include_deleted))
            return real(user_id, include_deleted=include_deleted)

        def cache_recorder(self, user_id):
            cached.append(user_id)
            return real_cached(self, user_id)

        client.get_user, cache.Cache.fetch_user = recorder, cache_recorder
        mods = {m: importlib.import_module("app." + m) for m in ASKS}
        for m, ask in ASKS.items():
            for flag in (False, True):
                for uid in (1, 2, 3):
                    del calls[:], cached[:]
                    try:
                        mods[m].run(uid, flag)
                    except Exception as e:
                        fails.append("app.%s.run(%d, %s) raised %r" % (m, uid, flag, e))
                        continue
                    ids = [uid, uid + 1] if m in ("search", "teams") else ([] if ask is None else [uid])
                    want = [(i, flag if ask == "flag" else ask) for i in ids]
                    if calls != want:
                        fails.append("app.%s.run(%d, %s) read users as %s, the old call asked for %s" % (m, uid, flag, calls, want))
                    if cached != ([uid] if m in ("notes", "cli") else []):
                        fails.append("app.%s.run(%d, %s) changed what the cache sees: %s" % (m, uid, flag, cached))
        del calls[:]
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            mods["cli"].main(["cli", "2"])
        if calls != [(2, True)] or "bob" not in out.getvalue():
            fails.append("app.cli.main read users as %s and printed %r; it asked for deleted users too" % (calls, out.getvalue()))
    except Exception as e:
        fails.append("the app is not usable: %r" % (e,))

p = subprocess.run([sys.executable, "-m", "unittest"], capture_output=True, encoding="utf-8", errors="replace", timeout=60)
ran = re.search(r"Ran (\d+) tests?", p.stdout + p.stderr)
if p.returncode != 0 or not ran:
    fails.append("the project's own tests do not pass: " + (p.stdout + p.stderr)[-300:])
for f in fails[:10]:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
