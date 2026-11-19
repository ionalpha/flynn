import ast
import os
import shutil
import subprocess
import sys

sys.path.insert(0, ".")
fails = []


def run(d):
    p = subprocess.run([sys.executable, "-m", "unittest", "discover", "-s", ".", "-p", "test*.py"],
                       cwd=d, capture_output=True, text=True, timeout=60)
    return p.returncode, p.stdout + p.stderr


for name in sorted(os.listdir("domain")) if os.path.isdir("domain") else []:
    if not name.endswith(".py"):
        continue
    for node in ast.walk(ast.parse(open(os.path.join("domain", name), encoding="utf-8").read())):
        names = []
        if isinstance(node, ast.Import):
            names = [a.name for a in node.names]
        elif isinstance(node, ast.ImportFrom) and node.module:
            names = [node.module]
        if any(n == "db" or n.startswith("db.") for n in names):
            fails.append("domain/" + name + " still imports db")
try:
    import app

    got = app.quote([("tea", 3), ("mug", 1)])
    if got != 2550:
        fails.append("app.quote for three teas and a mug is %r, want 2550" % (got,))
except Exception as e:
    fails.append("app.quote could not be exercised: %r" % (e,))
rc, out = run(".")
if rc != 0:
    fails.append("the tests fail on the result: " + out[-300:])
cases = [
    ("domain/zz_case.py", "from db import session\n", True, "domain importing from db"),
    ("domain/zz_case.py", "import db.session\n", True, "domain importing db.session"),
    ("domain/zz_case.py", "def f():\n    from db.models import Row\n    return Row\n", True, "a function in domain importing db.models"),
    ("domain/zz_case.py", "import dbtools\n", False, "domain importing dbtools, a different module"),
    ("db/zz_case.py", "from domain import pricing\n", False, "db importing domain, the allowed direction"),
]
for i, (path, code, should_fail, what) in enumerate(cases):
    d = "_mut_%d" % i
    shutil.copytree(".", d, ignore=shutil.ignore_patterns("_mut_*", "__pycache__", ".git"))
    with open(os.path.join(d, path), "w", newline="\n") as f:
        f.write(code)
    rc, out = run(d)
    if should_fail and (rc == 0 or "AssertionError" not in out):
        fails.append("the tests do not fail with an assertion on " + what)
    if not should_fail and rc != 0:
        fails.append("the tests fail on " + what + ": " + out[-300:])
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
