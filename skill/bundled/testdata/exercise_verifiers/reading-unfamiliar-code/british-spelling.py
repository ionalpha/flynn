import json
import subprocess
import sys

fails = []
ROWS = [("A", "new"), ("B", "paid"), ("C", "canceled"), ("D", "cancelled"), ("E", "shipped")]
with open("_v_orders.jsonl", "w", encoding="utf-8") as f:
    for i, s in ROWS:
        f.write(json.dumps({"id": i, "status": s}) + "\n")


def cli(*args):
    p = subprocess.run([sys.executable, "cli.py"] + list(args), capture_output=True, encoding="utf-8",
                       errors="replace", timeout=30)
    return p.returncode, p.stdout.splitlines(), p.stderr[-200:]


# Rows stored with the old spelling still count; a status nobody knows must not vanish from the totals.
rc, out, err = cli("summary", "_v_orders.jsonl")
want = ["new 1", "paid 1", "shipped 1", "delivered 0", "cancelled 2"]
if rc != 0 or out != want:
    fails.append("summary printed %r (rc %d %s), want %r" % (out, rc, err, want))
for oid, rc_want, lines in (("A", 0, ["A: new -> cancelled", "subject: Order A cancelled"]),
                            ("B", 0, ["B: paid -> cancelled", "subject: Order B cancelled"]),
                            ("E", 1, None), ("C", 1, None), ("D", 1, None)):
    rc, out, err = cli("cancel", "_v_orders.jsonl", oid)
    if rc != rc_want or (lines is not None and out != lines):
        fails.append("cancel %s gave rc %d %r %s; want rc %d %r" % (oid, rc, out, err, rc_want, lines))

p = subprocess.run([sys.executable, "-m", "unittest", "discover", "-s", ".", "-p", "test*.py"],
                   capture_output=True, encoding="utf-8", errors="replace", timeout=60)
if p.returncode != 0:
    fails.append("the project's own tests fail: " + p.stderr[-300:])
try:
    n = open("test_orders.py", encoding="utf-8").read().count("def test_")
    if n < 4:
        fails.append("test_orders.py has %d tests, it had 4" % n)
except OSError:
    fails.append("test_orders.py was removed")
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
