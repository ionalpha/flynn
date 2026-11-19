import ast
import os
import shutil
import subprocess
import sys

sys.path.insert(0, ".")
fails = []


def suite(cwd):
    return subprocess.run(
        [sys.executable, "-m", "unittest", "discover", "-q"], cwd=cwd, capture_output=True
    ).returncode


def definitions(root):
    found = []
    for d, dirs, files in os.walk(root):
        dirs[:] = [x for x in dirs if x not in ("__pycache__", ".git", "tests") and not x.startswith("_probe")]
        for f in files:
            if f.endswith(".py"):
                tree = ast.parse(open(os.path.join(d, f), encoding="utf-8").read())
                for n in ast.walk(tree):
                    if isinstance(n, (ast.FunctionDef, ast.AsyncFunctionDef)) and n.name == "parse_money":
                        found.append(os.path.normpath(os.path.join(d, f)).replace(os.sep, "/"))
    return found


def probe(name, rel):
    d = "_probe_" + name
    shutil.rmtree(d, ignore_errors=True)
    shutil.copytree(".", d, ignore=shutil.ignore_patterns("__pycache__", "_probe_*", ".git"))
    try:
        clean = suite(d)
        target = os.path.join(d, rel)
        os.makedirs(os.path.dirname(target), exist_ok=True)
        with open(target, "w") as f:
            f.write("def parse_money(text):\n    return 0\n")
        dirty = suite(d)
    finally:
        shutil.rmtree(d, ignore_errors=True)
    return clean, dirty


if suite(".") != 0:
    fails.append("the suite fails")
defs = definitions(".")
if defs != ["billing/money.py"]:
    fails.append("parse_money must be defined once, in billing/money.py, found %r" % defs)
try:
    from billing.money import parse_money as p
    from ingest.load import load_total

    if not (p("12.50") == 1250 and p("1,234.50") == 123450 and p("7") == 700):
        fails.append("the surviving parse_money changed behaviour")
    if p("(12.50)") != -1250 or load_total(["(1.00)", "2.50"]) != 150:
        fails.append("parenthesised negatives, which only the deleted copy handled, were lost")
except Exception as e:
    fails.append("import or call failed: %r" % (e,))
for name, rel in (("same_place", "ingest/amounts.py"), ("elsewhere", "reports/money_helpers.py")):
    clean, dirty = probe(name, rel)
    if clean != 0:
        fails.append("the tree copy used to probe %s does not pass its own suite" % name)
    elif dirty == 0:
        fails.append("a new parse_money in %s passes the suite: nothing stops the next copy" % rel)
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
