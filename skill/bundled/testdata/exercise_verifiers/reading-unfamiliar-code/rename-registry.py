import hashlib
import os
import subprocess
import sys

sys.path.insert(0, ".")


def h(p):
    return hashlib.sha256(open(p, "rb").read().replace(b"\r\n", b"\n")).hexdigest()


fails = []
if h("rows.csv") != "976774e30222abf6eb5822e12d5e8211d9be0fc3c62b36a2fb61d2de783f185c":
    fails.append("rows.csv was edited")
for d, dirs, files in os.walk("."):
    dirs[:] = [x for x in dirs if x not in (".git", "__pycache__")]
    for f in files:
        p = os.path.join(d, f)
        try:
            text = open(p, encoding="utf-8").read()
        except (OSError, UnicodeDecodeError):
            continue
        if "parse_amount" in text:
            fails.append("the old name is still in " + os.path.normpath(p))
try:
    import amounts

    for text, want in (("12.50", 1250), ("0.99", 99), ("3", 300), ("100.05", 10005)):
        if amounts.parse_cents(text) != want:
            fails.append("parse_cents(%r) is not %d" % (text, want))
except Exception as e:
    fails.append("amounts.parse_cents is not usable: %r" % (e,))
p = subprocess.run([sys.executable, "importer.py", "rows.csv"], capture_output=True, text=True, timeout=30)
if p.stdout.strip() != "imported 4 skipped 1":
    fails.append("the importer printed %r, want 'imported 4 skipped 1'" % p.stdout.strip())
p = subprocess.run(
    [sys.executable, "-m", "unittest", "discover", "-s", ".", "-p", "test*.py"],
    capture_output=True, text=True, timeout=60,
)
if p.returncode != 0:
    fails.append("the project's own tests fail: " + p.stderr[-300:])
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
