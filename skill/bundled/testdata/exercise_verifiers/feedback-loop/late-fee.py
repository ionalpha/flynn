import os
import shutil
import subprocess
import sys

SHIPPED = '"""Late fees for returned books."""\nfrom datetime import date\n\nPER_DAY = 25\nCAP = 500\n\n\ndef late_fee(due, returned):\n    """The fee in cents for a book due on due and returned on returned."""\n    if returned < due:\n        return 0\n    days = (returned - due).days + 1\n    return min(days * PER_DAY, CAP)\n'
fails = []
env = dict(os.environ, PYTHONDONTWRITEBYTECODE="1")


def clear():
    for root, dirs, _ in os.walk("."):
        if "__pycache__" in dirs:
            shutil.rmtree(os.path.join(root, "__pycache__"), ignore_errors=True)


def run(*args):
    clear()
    return subprocess.run([sys.executable] + list(args), env=env, capture_output=True, encoding="utf-8", errors="replace", timeout=60)


probe = (
    "from datetime import date as d; from fees import late_fee as f; "
    "print([f(d(2026, 1, 10), d(2026, 1, n)) for n in (9, 10, 11, 14)], f(d(2026, 1, 1), d(2026, 3, 1)))"
)
p = run("-c", probe)
if p.stdout.strip() != "[0, 0, 25, 100] 500":
    fails.append("fees for a day early, on time, 1 and 4 days late, then very late should be [0, 0, 25, 100] 500, got " + (p.stdout + p.stderr)[-300:])
p = run("check.py")
if p.returncode != 0:
    fails.append("python check.py fails: " + (p.stdout + p.stderr)[-300:])
if not os.path.exists("fees.py") or not os.path.exists("tests/fees_test.py"):
    fails.append("fees.py or tests/fees_test.py was removed")
else:
    with open("fees.py", "rb") as f:
        fixed = f.read()
    with open("tests/fees_test.py", "rb") as f:
        tests = f.read()
    try:
        with open("fees.py", "w", encoding="utf-8") as f:
            f.write(SHIPPED)
        if run("check.py").returncode == 0:
            fails.append("with the bug put back, python check.py still passes: no test that it runs covers the fix")
    finally:
        with open("fees.py", "wb") as f:
            f.write(fixed)
    try:
        with open("tests/fees_test.py", "ab") as f:
            f.write(b"\n\nclass Canary(unittest.TestCase):\n    def test_canary(self):\n        self.fail()\n")
        if run("check.py").returncode == 0:
            fails.append("python check.py no longer runs tests/fees_test.py")
    finally:
        with open("tests/fees_test.py", "wb") as f:
            f.write(tests)
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
