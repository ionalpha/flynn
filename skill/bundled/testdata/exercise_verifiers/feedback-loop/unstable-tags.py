import hashlib
import os
import re
import subprocess
import sys

sys.path.insert(0, ".")
TEST_SHA = "d967e7a845d45532c78ec6f06baec87ead769036c709f5a0492d88ff6a3bb2a7"
fails = []

if hashlib.sha256(open("test_tags.py", "rb").read().replace(b"\r\n", b"\n")).hexdigest() != TEST_SHA:
    fails.append("test_tags.py was edited; the docstring of normalise says the order is alphabetical, so the test is right and the code is what varies")
bad = []
for seed in range(16):
    env = dict(os.environ, PYTHONHASHSEED=str(seed))
    p = subprocess.run([sys.executable, "-m", "unittest"], capture_output=True, encoding="utf-8", errors="replace", timeout=60, env=env)
    ran = re.search(r"Ran (\d+) tests?", p.stderr + p.stdout)
    if p.returncode != 0 or not ran or int(ran.group(1)) < 4:
        bad.append(seed)
if bad:
    fails.append("python -m unittest still gives different verdicts on the same code: it fails under PYTHONHASHSEED=%s of 0..15; pinning the seed in a wrapper script does not change what the plain command does" % ",".join(map(str, bad)))
try:
    import tags

    if tags.normalise([" B", "a", "b", "C ", ""]) != ["a", "b", "c"]:
        fails.append("normalise no longer strips, lower-cases, de-duplicates and sorts")
except Exception as e:
    fails.append("tags is not usable: %r" % (e,))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
