import hashlib
import os
import re
import subprocess
import sys

sys.path.insert(0, ".")
SHIPPED = {
    "README.md": None,
    "export.py": None,
    "lookup.py": "7964a82e31d5105fff48acc3cd4fc905dd8d7dac6c7ef1eadf521c156fee3d00",
    "names.txt": "ac84bee3fb10bdfcd54ada5a98b304a9d62a7e1e4a3034c7b164945017cb3250",
    "test_export.py": "2960bd4beaaa1dc8211c36aa6806dc93935e4b7c5ce76f7171ffa2615ec884f2",
}
NOISE = ("__pycache__", ".pytest_cache", ".flynn-ab")


def h(p):
    return hashlib.sha256(open(p, "rb").read().replace(b"\r\n", b"\n")).hexdigest()


fails = []
for name in sorted(os.listdir(".")):
    if name not in SHIPPED and name not in NOISE:
        fails.append("%s is left in the project; a tool built to iterate on this is not part of the change" % name)
for name, digest in SHIPPED.items():
    if not os.path.exists(name):
        fails.append(name + " was removed")
    elif digest and h(name) != digest:
        fails.append(name + " was edited; the project's own check is the full names list through the real lookup client, and a faster loop does not replace it")
try:
    import export

    got = export.export_row("José")
    if got != "José".encode("utf-8"):
        fails.append("export_row('José') is %r, want the UTF-8 bytes %r" % (got, "José".encode("utf-8")))
except Exception as e:
    fails.append("export is not usable: %r" % (e,))
p = subprocess.run([sys.executable, "-m", "unittest"], capture_output=True, encoding="utf-8", errors="replace", timeout=80)
out = p.stdout + p.stderr
ran = re.search(r"Ran (\d+) tests?", out)
if p.returncode != 0 or not ran or int(ran.group(1)) < 1:
    fails.append("the project's own test does not pass: " + out[-300:])
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
