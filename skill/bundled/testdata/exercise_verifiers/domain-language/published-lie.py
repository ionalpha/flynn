import ast
import hashlib
import inspect
import os
import re
import subprocess
import sys
import warnings

sys.path.insert(0, ".")
fails = []
NOW = "2026-03-01"
KEEP = {"db.py": "3ed7377e8a2c853c", "schema.sql": "8b97c5b0f660fcb6"}
ALLOWED = {"README.md", "CHANGELOG.md", "schema.sql", "db.py", "sessions.py", "jobs.py", "maintenance.py"}
BAD = {"purge", "delete", "remove", "clean", "cleanup", "prune", "drop"}


def words(name):
    return {w.lower() for w in re.split(r"[^A-Za-z0-9]+", re.sub(r"([a-z0-9])([A-Z])", r"\1 \2", name)) if w}


def used(tree):
    return {n.id for n in ast.walk(tree) if isinstance(n, ast.Name)} | {n.attr for n in ast.walk(tree) if isinstance(n, ast.Attribute)}


for d, ds, fs in os.walk("."):
    ds[:] = [x for x in ds if not x.startswith(".") and x != "__pycache__"]
    for f in fs:
        p = os.path.relpath(os.path.join(d, f), ".").replace(os.sep, "/")
        if p not in ALLOWED and not f.startswith("test") and not f.endswith(".pyc"):
            fails.append("new file %s" % p)
for f, h in KEEP.items():
    if hashlib.sha256(open(f, "rb").read().replace(b"\r\n", b"\n")).hexdigest()[:16] != h:
        fails.append("%s was edited" % f)
new = None
try:
    tree = ast.parse(open("sessions.py", encoding="utf-8").read())
    top = {n.name: n for n in tree.body if isinstance(n, ast.FunctionDef)}
    cands = [k for k in top if k not in ("purge_old_sessions", "active_sessions") and "archive" in words(k) and not words(k) & BAD]
    allv = []
    for n in tree.body:
        if isinstance(n, ast.Assign) and any(getattr(t, "id", "") == "__all__" for t in n.targets):
            allv = [e.value for e in n.value.elts]
    if "purge_old_sessions" not in top or "purge_old_sessions" not in allv:
        fails.append("purge_old_sessions left the published package with no deprecation period")
    if not cands:
        fails.append("no function says archive; the only name on offer says purge")
    else:
        new = cands[0]
        if new not in allv:
            fails.append("%s is not in __all__" % new)
        if "purge_old_sessions" in top:
            old = top["purge_old_sessions"]
            called = {getattr(c.func, "id", getattr(c.func, "attr", "")) for c in ast.walk(old) if isinstance(c, ast.Call)}
            if new not in called or "warn" not in called:
                fails.append("purge_old_sessions must delegate to %s and warn" % new)
    for path in ("jobs.py", "maintenance.py"):
        u = used(ast.parse(open(path, encoding="utf-8").read()))
        if "purge_old_sessions" in u or (new and new not in u):
            fails.append("%s must call %s, not the deprecated name" % (path, new))
except Exception as e:
    fails.append("cannot read sessions.py: %r" % (e,))
try:
    import db
    import jobs
    import maintenance
    import sessions

    def state():
        live = [r[0] for r in db.CONN.execute("SELECT session_id FROM sessions ORDER BY 1")]
        return live, [tuple(r) for r in db.CONN.execute("SELECT * FROM session_archive ORDER BY 1")]

    want = (["s2", "s4"], [("s1", "u1", "2026-01-01", NOW), ("s3", "u1", "2025-12-15", NOW)])
    if new:
        fn = getattr(sessions, new)
        sig = inspect.signature(fn)
        if list(sig.parameters) != ["now", "days"] or sig.parameters["days"].default != 30:
            fails.append("%s changed the signature" % new)
        db.seed()
        n = fn(NOW)
        if n != 2 or state() != want:
            fails.append("%s returned %r and left %r" % (new, n, state()))
        db.seed()
        fn(NOW, 5)
        if state()[0] != ["s4"]:
            fails.append("%s ignores days" % new)
    db.seed()
    with warnings.catch_warnings(record=True) as seen:
        warnings.simplefilter("always")
        n = sessions.purge_old_sessions(NOW)
    if n != 2 or state() != want:
        fails.append("purge_old_sessions did not archive: %r %r" % (n, state()))
    dep = [str(w.message) for w in seen if issubclass(w.category, DeprecationWarning)]
    if not dep:
        fails.append("calling purge_old_sessions does not warn")
    elif not re.search(r"\d+\.\d+|\d{4}-\d{2}-\d{2}", dep[0]) or not new or new not in dep[0]:
        fails.append("the warning %r names no version or date, or not the new name" % dep[0])
    for label, call in (("jobs.nightly", lambda: jobs.nightly(NOW)), ("maintenance.weekly", lambda: maintenance.weekly(NOW))):
        db.seed()
        with warnings.catch_warnings():
            warnings.simplefilter("error")
            try:
                got = call()
            except Exception as e:
                got = repr(e)
        if got != 2 or not state()[1]:
            fails.append("%s gave %r" % (label, got))
except Exception as e:
    fails.append("the modules could not be exercised: %r" % (e,))
r = subprocess.run([sys.executable, "-m", "unittest", "discover"], capture_output=True, text=True)
if r.returncode != 0:
    fails.append("the suite fails: " + r.stderr.strip()[-300:])
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
