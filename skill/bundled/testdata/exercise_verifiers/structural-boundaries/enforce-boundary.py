import ast
import os
import shutil
import subprocess
import sys

sys.path.insert(0, ".")
fails = []


def run(d):
    p = subprocess.run([sys.executable, "-m", "unittest", "discover", "-s", ".", "-p", "test*.py"],
                       cwd=d, capture_output=True, encoding="utf-8", errors="replace", timeout=60)
    return p.returncode, p.stdout + p.stderr


for root, _, files in os.walk("domain"):
    for name in sorted(files):
        if not name.endswith(".py"):
            continue
        p = os.path.join(root, name)
        for node in ast.walk(ast.parse(open(p, encoding="utf-8").read())):
            mods = []
            if isinstance(node, ast.Import):
                mods = [a.name for a in node.names]
            elif isinstance(node, ast.ImportFrom) and node.module:
                mods = [node.module]
            elif isinstance(node, ast.Constant) and isinstance(node.value, str) and " " not in node.value:
                mods = [node.value]
            if any(n == "db" or n.startswith("db.") for n in mods):
                fails.append(p + " reaches db, by an import or by naming the module")
PATCHED = """
import db.session
db.session.current_rates = lambda: {"tea": 1, "mug": 2}
import app
print(app.quote([("tea", 3), ("mug", 1)]))
"""
try:
    got = subprocess.run([sys.executable, "-c", PATCHED], capture_output=True, encoding="utf-8", errors="replace", timeout=60).stdout.strip()
    if got != "5":
        fails.append("app.quote does not follow the rates db hands out: with tea at 1 and mug at 2 it gave %r, want 5" % (got,))
except Exception as e:
    fails.append("app.quote could not be exercised against other rates: %r" % (e,))
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
    ("domain/sub/zz_case.py", "from db import session\n", True, "a nested domain package importing db"),
    ("db/zz_case.py", "from domain import pricing\n", False, "db importing domain, the allowed direction"),
]
for i, (path, code, should_fail, what) in enumerate(cases):
    d = "_mut_%d" % i
    shutil.copytree(".", d, ignore=shutil.ignore_patterns("_mut_*", "__pycache__", ".git"))
    os.makedirs(os.path.dirname(os.path.join(d, path)), exist_ok=True)
    with open(os.path.join(d, path), "w", newline="\n", encoding="utf-8") as f:
        f.write(code)
    rc, out = run(d)
    if should_fail and (rc == 0 or "AssertionError" not in out):
        fails.append("the tests do not fail with an assertion on " + what)
    if not should_fail and rc != 0:
        fails.append("the tests fail on " + what + ": " + out[-300:])
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
