import os
import re
import shutil
import subprocess
import sys

sys.path.insert(0, ".")
fails = []

VALID = (("1h30m", 5400), ("90s", 90), ("2h", 7200), ("1h1m1s", 3661), ("0s", 0), ("100m", 6000), ("007s", 7))
INVALID = ("", "h", "30", "1m1h", "1h1h", "-5s", "1.5h", " 1h", "1h ", "1h 30m", "1H", "1h\n", "1d", "s1")
try:
    import duration

    for text, want in VALID:
        got = duration.parse_duration(text)
        if got != want:
            fails.append("parse_duration(%r) is %r, want %d" % (text, got, want))
    for text in INVALID:
        try:
            got = duration.parse_duration(text)
            fails.append("parse_duration(%r) returned %r instead of raising" % (text, got))
        except ValueError:
            pass
except Exception as e:
    fails.append("duration.parse_duration is not usable: %r" % (e,))

REGEX = """import re

_P = re.compile(r"%(pat)s")


def parse_duration(text):
    %(pre)s
    m = _P.match(text)
    if %(chk)s m is None:
        raise ValueError(text)
    h, mi, s = (int(g or 0) for g in m.groups())
    return h * 3600 + mi * %(mm)d + s
"""
SCAN = """import re


def parse_duration(text):
    parts = re.findall(r"([0-9]+)([hms])", text)
    u = [b for _, b in parts]
    if not parts or "".join(a + b for a, b in parts) != text or not (%(dup)s) or not (%(order)s):
        raise ValueError(text)
    return sum(int(a) * {"h": 3600, "m": 60, "s": 1}[b] for a, b in parts)
"""
PAT = r"(?:([0-9]+)h)?(?:([0-9]+)m)?(?:([0-9]+)s)?\Z"
GOOD = dict(pat=PAT, pre="pass", chk="not text or", mm=60)
SCAN_GOOD = dict(dup="len(set(u)) == len(u)", order="u == sorted(u, key='hms'.index)")
MUTANTS = {
    "accepts the empty string": (REGEX, dict(GOOD, chk="")),
    "accepts a trailing newline": (REGEX, dict(GOOD, pat=PAT.replace(r"\Z", "$"))),
    "accepts upper case": (REGEX, dict(GOOD, pre="text = text.lower()")),
    "accepts surrounding spaces": (REGEX, dict(GOOD, pre="text = text.strip()")),
    "accepts a bare number": (REGEX, dict(GOOD, pat=PAT.replace("s)?", "s?)?"))),
    "accepts a sign": (REGEX, dict(GOOD, pat="[+-]?" + PAT)),
    "miscounts a minute": (REGEX, dict(GOOD, mm=50)),
    "accepts units out of order": (SCAN, dict(SCAN_GOOD, order="True")),
    "accepts a repeated unit": (SCAN, dict(SCAN_GOOD, dup="True")),
}


def run(d):
    p = subprocess.run(
        [sys.executable, "-m", "unittest", "test_duration"],
        cwd=d, capture_output=True, encoding="utf-8", errors="replace", timeout=30,
    )
    return p.returncode, p.stderr


if not os.path.exists("test_duration.py"):
    fails.append("there is no test_duration.py")
else:
    rc, out = run(".")
    ran = re.search(r"Ran (\d+) test", out)
    if rc != 0:
        fails.append("the tests fail on the implementation: " + out[-300:])
    elif not ran or int(ran.group(1)) < 1:
        fails.append("no test ran")
    else:
        for what, (tpl, parts) in MUTANTS.items():
            d = "_mut_%d" % len(os.listdir("."))
            shutil.copytree(".", d, ignore=shutil.ignore_patterns("_mut_*", "__pycache__", ".git"))
            with open(os.path.join(d, "duration.py"), "w", newline="\n", encoding="utf-8") as f:
                f.write(tpl % parts)
            if run(d)[0] == 0:
                fails.append("the tests still pass when the code " + what)
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
