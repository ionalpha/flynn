import ast
import os
import subprocess
import sys

sys.path.insert(0, ".")
sys.dont_write_bytecode = True
fails = []

try:

    import csv

    import client
    import export

    client.ORDERS[:] = [
        {"id": "o%d" % i, "customer": "c%d" % (i % 3), "cents": 100 + i, "deleted": i in (3, 150) or 200 <= i < 300}
        for i in range(450)
    ]
    want = [["id", "customer", "cents"]]
    want += [[o["id"], o["customer"], str(o["cents"])] for o in client.ORDERS if not o["deleted"]]
    try:
        export.export_csv(".out.csv")
        with open(".out.csv", newline="") as f:
            got = list(csv.reader(f))
    finally:
        if os.path.exists(".out.csv"):
            os.remove(".out.csv")
    if got != want:
        fails.append("the export holds %d lines, want the header and all %d live orders" % (len(got), len(want) - 1))
except Exception as e:
    fails.append("the code could not be exercised: %r" % (e,))
r = subprocess.run([sys.executable, "-B", "-m", "unittest", "discover"], capture_output=True, text=True)
if r.returncode != 0:
    fails.append("the suite fails: " + r.stderr.strip()[-300:])
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
