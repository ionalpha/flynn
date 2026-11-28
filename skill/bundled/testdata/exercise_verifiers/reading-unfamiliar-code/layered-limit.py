import os
import subprocess
import sys

fails = []


def cli(env, *args):
    e = {k: v for k, v in os.environ.items() if k != "APP_ENV"}
    if env:
        e["APP_ENV"] = env
    p = subprocess.run([sys.executable, "cli.py"] + list(args), capture_output=True, env=e,
                       encoding="utf-8", errors="replace", timeout=30)
    return p.stdout.strip() if p.returncode == 0 else "failed: " + p.stderr[-200:]


# An environment with no file of its own ("staging") takes the defaults.
for env in (None, "dev", "prod", "staging"):
    for args, want in ((("export",), "exported 2000 rows"), (("page", "9999"), "page 500 rows"), (("page", "40"), "page 40 rows")):
        got = cli(env, *args)
        if got != want:
            fails.append("APP_ENV=%s cli.py %s printed %r, want %r" % (env, " ".join(args), got, want))

p = subprocess.run([sys.executable, "-m", "unittest", "discover", "-s", ".", "-p", "test*.py"],
                   capture_output=True, encoding="utf-8", errors="replace", timeout=60)
if p.returncode != 0:
    fails.append("the project's own tests fail: " + p.stderr[-300:])
try:
    tests = open("test_export.py", encoding="utf-8").read()
    if tests.count("def test_") < 3 or "2000" not in tests:
        fails.append("test_export.py lost tests or does not test the new limit")
except OSError:
    fails.append("test_export.py was removed")
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
