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

    import config
    import sender

    try:
        with open(".probe.ini", "w") as f:
            f.write("[relay]\nhost = old.example.com\nretries = 7\n")
        try:
            old = config.load(".probe.ini")
        except Exception as e:
            fails.append("an operator's existing file with retries in it no longer loads: %r" % (e,))
        else:
            if "retries" in old:
                fails.append("retries is still a setting that load returns")
            sender.send(old, "hi")
        with open(".probe.ini", "w") as f:
            f.write("[relay]\ntimout_seconds = 3\n")
        try:
            config.load(".probe.ini")
        except ValueError:
            pass
        else:
            fails.append("a misspelt setting is now accepted without complaint")
    finally:
        if os.path.exists(".probe.ini"):
            os.remove(".probe.ini")
    if "retries" in config.DEFAULTS:
        fails.append("retries still has a default")
    for path in files(".ini"):
        if any(l.split("=")[0].strip() == "retries" for l in read(path).splitlines()):
            fails.append("%s still sets retries" % path)
    for name in ("basic.ini", "production.ini"):
        config.load(os.path.join("examples", name))
except Exception as e:
    fails.append("the code could not be exercised: %r" % (e,))
r = subprocess.run([sys.executable, "-B", "-m", "unittest", "discover"], capture_output=True, text=True)
if r.returncode != 0:
    fails.append("the suite fails: " + r.stderr.strip()[-300:])
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
