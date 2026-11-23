import ast
import os
import shutil
import subprocess
import sys
import time

sys.path.insert(0, ".")
fails = []


def sleeps(tree):
    for node in ast.walk(tree):
        if isinstance(node, ast.ImportFrom) and node.module == "time" and any(a.name == "sleep" for a in node.names):
            return True
        if isinstance(node, ast.Attribute) and node.attr == "sleep" and isinstance(node.value, ast.Name) and node.value.id == "time":
            return True
    return False


tests = []
for root, dirs, files in os.walk("."):
    dirs[:] = [d for d in dirs if d not in (".git", ".flynn-ab", "__pycache__") and not d.startswith("_mut")]
    tests += [os.path.join(root, f) for f in files if f.startswith("test") and f.endswith(".py")]
for p in tests:
    if sleeps(ast.parse(open(p, encoding="utf-8").read())):
        fails.append(p + " still sleeps on the wall clock; move an injected clock instead")


def run(d):
    p = subprocess.run([sys.executable, "-m", "unittest", "discover", "-s", ".", "-p", "test*.py"],
                       cwd=d, capture_output=True, encoding="utf-8", errors="replace", timeout=60)
    return p.returncode, p.stdout + p.stderr


rc, out = run(".")
if rc != 0:
    fails.append("the tests fail on the result: " + out[-300:])


def mutate(method, body):
    tree = ast.parse(open("lease.py", encoding="utf-8").read())
    for node in ast.walk(tree):
        if isinstance(node, ast.ClassDef) and node.name == "Lease":
            for fn in node.body:
                if isinstance(fn, ast.FunctionDef) and fn.name == method:
                    fn.body = ast.parse(body).body
                    return ast.unparse(tree)
    return None


for i, (method, body, what) in enumerate((
    ("expired", "return False", "a lease that never expires"),
    ("expired", "return True", "a lease that is expired from the start"),
    ("renew", "pass", "a renew that does nothing"),
)):
    src = mutate(method, body)
    if src is None:
        fails.append("lease.py no longer has Lease." + method)
        continue
    d = "_mut_%d" % i
    shutil.copytree(".", d, ignore=shutil.ignore_patterns("_mut_*", "__pycache__", ".git"))
    with open(os.path.join(d, "lease.py"), "w", newline="\n", encoding="utf-8") as f:
        f.write(src)
    rc, out = run(d)
    if rc == 0 or "AssertionError" not in out:
        fails.append("the tests do not fail with an assertion on " + what)
try:
    import lease

    if lease.Lease(1000).expired():
        fails.append("a fresh 1000 second lease is already expired with the default clock")
    short = lease.Lease(0.05)
    time.sleep(0.1)
    if not short.expired():
        fails.append("a 0.05 second lease is not expired 0.1 seconds later; the default clock must be the real one")
except Exception as e:
    fails.append("Lease could not be exercised with its default clock: %r" % (e,))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
