import tempfile
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

try:
    import messages
    from accounts import create_account, delete_account
    from store import Store

    with tempfile.TemporaryDirectory(dir=".") as d:
        s = Store(d)
        for u in ("ann", "bob"):
            create_account(s, u)
        s.upload("bob", "logo.png", b"same bytes")
        s.upload("ann", "logo-copy.png", b"same bytes")
        own = s.upload("ann", "diary.txt", b"only ann has this")
        s.upload("ann", "plan.txt", b"sent to bob")
        messages.send(s, "ann", "bob", "plan.txt")
        delete_account(s, "ann")
        for what, fn, want in (("bob's own upload of the same content", lambda: s.read("bob", "logo.png"), b"same bytes"),
                               ("the attachment bob received", lambda: messages.attachment(s, "bob", 0), b"sent to bob")):
            try:
                if fn() != want:
                    fails.append(what + " changed")
            except Exception as e:
                fails.append("%s is gone: %r" % (what, e))
        if os.path.exists(s.blob_path(own)):
            fails.append("a file only the closed account had is still on disk")
except Exception as e:
    fails.append("closing an account is not usable: %r" % (e,))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
