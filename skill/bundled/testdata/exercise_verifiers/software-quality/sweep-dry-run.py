import json
import os
import shutil
import subprocess
import sys

NOTICE = "bo: b.txt expired and will be removed\n"
START = {
    "a.txt": {"owner": "ana", "expires": "2026-03-01", "marked": "2026-03-09"},
    "b.txt": {"owner": "bo", "expires": "2026-03-04"},
    "c.txt": {"owner": "cy", "expires": "2026-05-01"},
}
fails = []


def build(root):
    os.makedirs(root)
    for name in START:
        with open(os.path.join(root, name), "w", encoding="utf-8") as f:
            f.write(name)
    with open(os.path.join(root, "catalog.json"), "w", encoding="utf-8") as f:
        json.dump(START, f)


def snapshot(root):
    out = {}
    for base, _, files in os.walk(root):
        for name in files:
            with open(os.path.join(base, name), "rb") as f:
                out[os.path.relpath(os.path.join(base, name), root)] = f.read()
    return out


def run(*args):
    return subprocess.run([sys.executable] + list(args), capture_output=True, encoding="utf-8", errors="replace", timeout=60)


# mkdtemp locks its directory to the creating user on recent Windows Pythons, which a confined process cannot write into.
tmp = os.path.join(os.getcwd(), ".verify-%d" % os.getpid())
try:
    root = os.path.join(tmp, "uploads")
    build(root)
    before = snapshot(root)
    p = run("sweep.py", root, "--today", "2026-03-10", "--dry-run")
    if p.returncode != 0:
        fails.append("sweep.py --dry-run failed: " + (p.stdout + p.stderr)[-300:])
    if "a.txt" not in p.stdout:
        fails.append("the dry run does not name a.txt, which tonight's run removes: %r" % p.stdout[-300:])
    changed = sorted(k for k in set(before) | set(snapshot(root)) if before.get(k) != snapshot(root).get(k))
    if changed:
        fails.append("the dry run changed %s; a preview must leave the directory as it found it, or owners are mailed and tonight's run behaves differently" % changed)

    # The real run still does what it did.
    root = os.path.join(tmp, "real")
    build(root)
    p = run("sweep.py", root, "--today", "2026-03-10")
    after = snapshot(root)
    entries = json.loads(after.get("catalog.json", b"{}").decode())
    if p.returncode != 0 or "a.txt" in after or "b.txt" not in after or "c.txt" not in after:
        fails.append("without the flag the run should remove a.txt and nothing else: " + (p.stdout + p.stderr)[-300:])
    if sorted(entries) != ["b.txt", "c.txt"] or entries["b.txt"].get("marked") != "2026-03-10":
        fails.append("without the flag the catalog should drop a.txt and mark b.txt, got %s" % entries)
    if after.get("outbox.log", b"").decode().replace("\r\n", "\n") != NOTICE:
        fails.append("without the flag bo gets one notice about b.txt, got %r" % after.get("outbox.log"))
finally:
    shutil.rmtree(tmp, ignore_errors=True)

p = run("-m", "unittest")
if p.returncode != 0:
    fails.append("the project's own tests fail: " + (p.stdout + p.stderr)[-300:])
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
