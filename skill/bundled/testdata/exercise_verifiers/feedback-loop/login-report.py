import os
import subprocess
import sys

LOG = """2026-04-01T09:00:00 web1 user=pat result=fail
2026-04-01T09:00:01 web2 user=Ann Marie Cole result=fail
2026-04-01T09:00:02 web2 user=Ann Marie Cole result=fail
2026-04-01T09:00:03 web2 user=Ann Marie Cole result=fail
2026-04-01T09:00:04 web3 user=kit ip=192.168.0.9 result=fail
2026-04-01T09:00:05 web3 user=kit ip=192.168.0.10 result=fail
2026-04-01T09:00:06 web3 user=Raj de Silva ip=172.16.3.3 result=fail
2026-04-01T09:00:07 web1 user=pat result=ok
2026-04-01T09:00:08 web1 healthcheck result=fail
"""
WANT = ["Ann Marie Cole: 3", "kit: 2", "Raj de Silva: 1", "pat: 1"]
fails = []
path = os.path.join(os.getcwd(), ".verify-%d.log" % os.getpid())


def run(*args):
    return subprocess.run([sys.executable] + list(args), capture_output=True, encoding="utf-8", errors="replace", timeout=60)


try:
    with open(path, "w", encoding="utf-8") as f:
        f.write(LOG)
    p = run("report.py", path)
    got = p.stdout.strip().splitlines()
    if p.returncode != 0 or got != WANT:
        fails.append("for lines like those in logs/sample.log the report should be %s, got %s" % (WANT, got or p.stderr[-300:]))
finally:
    if os.path.exists(path):
        os.remove(path)
if not os.path.exists("logs/sample.log"):
    fails.append("logs/sample.log was removed")
p = run("-m", "unittest")
if p.returncode != 0:
    fails.append("the project's own tests fail: " + (p.stdout + p.stderr)[-300:])
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
