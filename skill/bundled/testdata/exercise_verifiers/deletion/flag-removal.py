import ast
import os
import subprocess
import sys

sys.path.insert(0, ".")
sys.dont_write_bytecode = True
fails = []


def files(ext):
    for root, dirs, names in os.walk("."):
        dirs[:] = [d for d in dirs if not d.startswith(".") and d != "__pycache__"]
        for n in names:
            if n.endswith(ext) and not n.startswith("."):
                yield os.path.join(root, n)[2:].replace(os.sep, "/")


def read(p):
    with open(p, encoding="utf-8", errors="replace") as f:
        return f.read()

try:

    import json

    for path in files(""):
        if not path.endswith(".pyc") and path != "CHANGELOG.md" and "new_checkout" in read(path):
            fails.append("%s still mentions new_checkout" % path)
    for path in files(".py"):
        for name in ("_flat_total", "OLD_FLAT_PERCENT_OFF"):
            if name in read(path):
                fails.append("%s still carries %s, which only the old checkout used" % (path, name))
    if json.loads(read("flags.json")) != {"gift_notes": False}:
        fails.append("flags.json should keep gift_notes as it was and drop the retired flag")
    import checkout
    import flags
    import refunds

    if flags.enabled("gift_notes") is not False:
        fails.append("flags.enabled no longer answers for gift_notes")
    got = (checkout.total([{"cents": 100, "qty": 10}, {"cents": 5, "qty": 3}]), refunds.partial_refund(25, 10))
    if got != (915, 3):
        fails.append("the live checkout or refunds changed: %r, want (915, 3)" % (got,))
    kept = open("checkout.py", "rb").read()
    try:
        with open("checkout.py", "ab") as f:
            f.write(b"\n\n_whole = total\n\n\ndef total(cart):\n    return _whole(cart) + 1\n")
        m = subprocess.run([sys.executable, "-B", "-m", "unittest", "discover"], capture_output=True, text=True)
        if m.returncode == 0:
            fails.append("no test notices a wrong total: the live checkout lost its coverage")
    finally:
        with open("checkout.py", "wb") as f:
            f.write(kept)
except Exception as e:
    fails.append("the code could not be exercised: %r" % (e,))
r = subprocess.run([sys.executable, "-B", "-m", "unittest", "discover"], capture_output=True, text=True)
if r.returncode != 0:
    fails.append("the suite fails: " + r.stderr.strip()[-300:])
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
