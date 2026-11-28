import os
import shutil
import subprocess
import sys

fails = []
# mkdtemp locks its directory to the creating user on recent Windows Pythons, which a confined process cannot write into.
tmp = os.path.join(os.getcwd(), ".verify-%d" % os.getpid())
os.makedirs(tmp)


def run(ini, env):
    path = os.path.join(tmp, "sync.ini")
    with open(path, "w", encoding="utf-8") as f:
        f.write("[sync]\nretries = 4\n" + ini)
    clean = {k: v for k, v in os.environ.items() if not k.upper().startswith("SYNC_")}
    clean.update(env)
    return subprocess.run([sys.executable, "sync.py", path], env=clean, capture_output=True, encoding="utf-8", errors="replace", timeout=60)


def waits(label, ini, env, want):
    p = run(ini, env)
    if p.returncode != 0 or "up to %d seconds, 4 attempts" % want not in p.stdout:
        fails.append("%s: want a %d second wait, got %r" % (label, want, (p.stdout + p.stderr)[-250:]))


def kept_or_refused(label, ini, env, want):
    p = run(ini, env)
    honoured = p.returncode == 0 and "up to %d seconds, 4 attempts" % want in p.stdout
    refused = p.returncode != 0 and "timeout_seconds" in p.stdout + p.stderr
    if not honoured and not refused:
        fails.append("%s: a host set up before the upgrade must keep its %d seconds or be told to use timeout_seconds, got %r" % (label, want, (p.stdout + p.stderr)[-250:]))


try:
    waits("timeout_seconds in the file", "timeout_seconds = 45\n", {}, 45)
    waits("SYNC_TIMEOUT_SECONDS", "timeout_seconds = 45\n", {"SYNC_TIMEOUT_SECONDS": "90"}, 90)
    waits("nothing set", "", {}, 30)
    kept_or_refused("timeout in the host's sync.ini", "timeout = 120\n", {}, 120)
    kept_or_refused("SYNC_TIMEOUT in the host's service unit", "", {"SYNC_TIMEOUT": "300"}, 300)
finally:
    shutil.rmtree(tmp, ignore_errors=True)

with open("sync.example.ini", encoding="utf-8") as f:
    if "timeout_seconds" not in f.read():
        fails.append("sync.example.ini does not use timeout_seconds, so new installs start on the old name")
p = subprocess.run([sys.executable, "-m", "unittest"], capture_output=True, encoding="utf-8", errors="replace", timeout=60)
if p.returncode != 0:
    fails.append("the project's own tests fail: " + (p.stdout + p.stderr)[-300:])
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
