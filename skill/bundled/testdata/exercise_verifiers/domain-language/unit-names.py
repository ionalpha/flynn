import ast
import hashlib
import os
import re
import subprocess
import sys
import time

sys.path.insert(0, ".")
fails = []
KEEP = {"cache.py": "eae7cc890f4d8869", "clock.py": "163d43320e992774", "client.py": "f3b8bbb623439446"}
ALLOWED = {"README.md", "cache.py", "clock.py", "client.py", "reports.py"}
UNIT = {"ms": 0.001, "millis": 0.001, "millisecond": 0.001, "milliseconds": 0.001, "s": 1, "sec": 1, "secs": 1, "second": 1, "seconds": 1,
        "min": 60, "mins": 60, "minute": 60, "minutes": 60, "h": 3600, "hr": 3600, "hrs": 3600, "hour": 3600, "hours": 3600}
RAW = {2, 3, 120, 180, 10800, 120000, 10800000}
for d, ds, fs in os.walk("."):
    ds[:] = [x for x in ds if not x.startswith(".") and x != "__pycache__"]
    for f in fs:
        p = os.path.relpath(os.path.join(d, f), ".").replace(os.sep, "/")
        if p not in ALLOWED and not f.startswith("test") and not f.endswith(".pyc"):
            fails.append("new file %s" % p)
for f, h in KEEP.items():
    if hashlib.sha256(open(f, "rb").read().replace(b"\r\n", b"\n")).hexdigest()[:16] != h:
        fails.append("%s was edited; the units of the existing calls are what they are" % f)


def ev(n, env):
    if isinstance(n, ast.Constant) and type(n.value) in (int, float):
        return n.value
    if isinstance(n, ast.Name):
        return env.get(n.id)
    if isinstance(n, ast.BinOp):
        a, b = ev(n.left, env), ev(n.right, env)
        ops = {ast.Add: lambda: a + b, ast.Sub: lambda: a - b, ast.Mult: lambda: a * b, ast.Div: lambda: a / b, ast.FloorDiv: lambda: a // b}
        if a is not None and b is not None and type(n.op) in ops:
            return ops[type(n.op)]()


try:
    tree = ast.parse(open("reports.py", encoding="utf-8").read())
    binds = []
    for n in ast.walk(tree):
        if isinstance(n, (ast.Assign, ast.AnnAssign)):
            t = n.targets[0] if isinstance(n, ast.Assign) else n.target
            if isinstance(t, ast.Name) and n.value is not None:
                binds.append((n.lineno, t.id, n.value))
        elif isinstance(n, ast.FunctionDef):
            pos = n.args.args[len(n.args.args) - len(n.args.defaults):]
            binds += [(n.lineno, a.arg, v) for a, v in zip(pos, n.args.defaults)]
    env = {}
    for _, name, rhs in sorted(binds, key=lambda b: b[0]):
        v = ev(rhs, env)
        if v is None:
            continue
        env[name] = v
        if v in RAW:
            us = [UNIT[w] for w in re.split(r"[^a-z]+", re.sub(r"([a-z])([A-Z])", r"\1_\2", name).lower()) if w in UNIT]
            if not us:
                fails.append("%s = %r carries no unit in its name" % (name, v))
            elif not any(v * u in (120, 10800) for u in us):
                fails.append("%s = %r: the name says one unit and the value is another" % (name, v))
    for n in ast.walk(tree):
        if isinstance(n, ast.Call):
            for a in list(n.args) + [k.value for k in n.keywords]:
                if not any(isinstance(x, ast.Name) for x in ast.walk(a)) and ev(a, {}) in (120, 180, 10800, 120000, 10800000):
                    fails.append("a bare %r passed to a call: its unit lives only in the author's head" % ev(a, {}))
except Exception as e:
    fails.append("cannot read reports.py: %r" % (e,))
try:
    slept = []
    time.sleep = slept.append
    import cache
    import client
    import clock
    import reports

    clock._sleep = slept.append
    cache.now = lambda: 1000.0

    def run(failures, name):
        calls, slept[:] = [], []
        cache.STORE.clear()

        def transport(url):
            calls.append(url)
            if len(calls) <= failures:
                raise client.FetchError("fail %d" % len(calls))
            return "BODY"

        client.TRANSPORT = transport
        try:
            got = reports.download("http://r/9")
        except client.FetchError as e:
            got = str(e)
        return got, len(calls), [round(s, 6) for s in slept], cache.STORE.get("http://r/9")

    for failures, want in (
        (0, ("BODY", 1, [], ("BODY", 11800.0))),
        (3, ("BODY", 4, [120.0] * 3, ("BODY", 11800.0))),
        (9, ("fail 5", 5, [120.0] * 4, None)),
    ):
        got = run(failures, failures)
        if got != want:
            fails.append("with %d failures: got %r, want %r" % (failures, got, want))
except Exception as e:
    fails.append("the modules could not be exercised: %r" % (e,))
r = subprocess.run([sys.executable, "-m", "unittest", "discover"], capture_output=True, text=True)
if r.returncode != 0:
    fails.append("the suite fails: " + r.stderr.strip()[-300:])
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
