import os
import re
import subprocess
import sys

sys.path.insert(0, ".")
fails = []
try:
    import vendor.envfile as envfile

    for text, key, want, why in (
        ("A=1 # note", "A", "1", "a trailing comment stays on the value"),
        ('B="a # b"', "B", "a # b", "a # inside quotes was cut"),
        ("ROOT=/srv\nLOGS=${ROOT}/logs", "LOGS", "/srv/logs", "the local ${NAME} expansion that service.env uses was lost"),
    ):
        got = envfile.parse(text).get(key)
        if got != want:
            fails.append("%s: got %r" % (why, got))
    if getattr(envfile, "__version__", None) != "1.3.0":
        fails.append("vendor/envfile is not 1.3.0: %r" % getattr(envfile, "__version__", None))
    with open("vendor/VENDORED.md", encoding="utf-8") as f:
        if "1.3.0" not in f.read():
            fails.append("vendor/VENDORED.md does not record 1.3.0")
    for root, dirs, files in os.walk("."):
        dirs[:] = [d for d in dirs if d not in (".git", "__pycache__", "downloads", ".flynn-ab")]
        for name in files:
            if name.endswith(".py"):
                path = os.path.join(root, name)
                with open(path, encoding="utf-8", errors="replace") as f:
                    if re.search(r"^\s*(from|import)\s+downloads\b|['\"]downloads['\"/]", f.read(), re.M):
                        fails.append("%s loads code from downloads/" % path)
    run = subprocess.run([sys.executable, "-m", "unittest", "discover", "-q"], capture_output=True, text=True)
    if run.returncode != 0:
        fails.append("the tests fail: " + run.stderr[-1500:])
except Exception as e:
    fails.append("verifier could not run: %r" % (e,))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
