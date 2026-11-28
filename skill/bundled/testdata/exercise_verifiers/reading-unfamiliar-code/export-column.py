import csv
import os
import shutil
import subprocess
import sys

sys.path.insert(0, ".")
sys.dont_write_bytecode = True
fails = []


def suite(d="."):
    p = subprocess.run([sys.executable, "-B", "-m", "unittest", "discover", "-s", ".", "-p", "test*.py"],
                       cwd=d, capture_output=True, encoding="utf-8", errors="replace", timeout=60)
    return p.returncode, p.stderr


rc, out = suite()
if rc != 0:
    fails.append("the project's tests fail: " + out[-300:])


def run(script):
    return subprocess.run([sys.executable, "-B", script], capture_output=True, encoding="utf-8", errors="replace", timeout=30)


p = run("export.py")
if p.returncode != 0:
    fails.append("export.py failed: " + p.stderr[-300:])
else:
    with open("out/orders.csv", newline="", encoding="utf-8") as f:
        rows = list(csv.reader(f))
    want = [["order_id", "customer", "email", "total_cents", "status"],
            ["A1", "Ada Park", "ada@example.com", "2500", "paid"],
            ["A2", "Bo Chen", "bo@example.com", "990", "open"],
            ["A3", "Bo Chen", "bo@example.com", "5000", "paid"]]
    if rows != want:
        fails.append("the export is %r" % (rows,))
    p = run("finance/reconcile.py")
    if p.stdout.strip() != "reconciled 2 orders, 7500 cents":
        fails.append("the nightly reconcile printed %r %s" % (p.stdout.strip(), p.stderr[-200:]))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
