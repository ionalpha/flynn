import hashlib
import subprocess
import sys


def h(p):
    return hashlib.sha256(open(p, "rb").read().replace(b"\r\n", b"\n")).hexdigest()


fails = []
if h("queue.jsonl") != "5153455678436c22e13dc67d939ffcbd9a3051e0a0aa7e9c8f90b39bdff5ea27":
    fails.append("queue.jsonl, persisted data, was edited")
if h("test_replay.py") != "51a5479004000bdfbe527555d32124d4a19bf9b6c8ca3311f862d6d6e67d48ed":
    fails.append("test_replay.py was edited")
r = subprocess.run([sys.executable, "replay.py"], capture_output=True, text=True)
if r.returncode != 0:
    fails.append("replay.py fails on the queue: " + r.stderr.strip().splitlines()[-1] if r.stderr.strip() else "replay.py fails")
else:
    try:
        total = float(r.stdout.strip())
    except ValueError:
        total = None
    if total != 176.0:
        fails.append("replay.py printed %r, want 176.0" % r.stdout.strip())
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
