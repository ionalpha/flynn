import hashlib
import hmac
import json
import os
import subprocess
import sys

fails = []


def h(p):
    return hashlib.sha256(open(p, "rb").read().replace(b"\r\n", b"\n")).hexdigest()[:16]


for name, want in (("fields.py", "0cc024283f3ab0fa"), ("partner-spec.md", "3a6e82afd97d40cc")):
    try:
        if h(name) != want:
            fails.append(name + " was edited; it is the partner's contract")
    except OSError:
        fails.append(name + " is gone")
try:
    tests = open("test_signing.py", encoding="utf-8").read()
except OSError:
    tests = ""
for need in ("def test_signature_matches_partner_example", "ee4c29bb2535446b0c5dc8dfb15d5d908c5010017a38a1175c07349c7c73fb16"):
    if need not in tests:
        fails.append("test_signing.py lost " + need + "; the partner's signature is the check")

RANK = ["id", "amount", "currency", "status"]
PAYLOADS = [
    {"id": "A-1", "amount": 5, "currency": "EUR", "status": "paid", "memo": "m", "note": "n", "trace_id": "x"},
    {"note": "n", "zeta": 1, "alpha": 2, "beta": 3, "id": "B-7", "sent_at": 1},
    {"status": "open", "memo": "a", "note": "b", "extra": "c", "region": "eu", "amount": 9},
]


def want(p):
    names = [n for n in p if n not in ("trace_id", "sent_at")]
    names = [n for n in RANK if n in names] + sorted(n for n in names if n not in RANK)
    text = "&".join("%s=%s" % (n, p[n]) for n in names)
    return [text, hmac.new(b"k", text.encode(), hashlib.sha256).hexdigest()]


PROBE = (
    "import json,sys\nsys.path.insert(0,'.')\nimport signing\n"
    "P=json.loads(sys.argv[1])\n"
    "print(json.dumps([[signing.canonical(p), signing.sign(p,'k')] for p in P]))\n"
)
for seed in range(10):
    env = dict(os.environ, PYTHONHASHSEED=str(seed))
    p = subprocess.run([sys.executable, "-c", PROBE, json.dumps(PAYLOADS)], env=env, capture_output=True,
                       encoding="utf-8", errors="replace", timeout=60)
    try:
        got = json.loads(p.stdout)
    except ValueError:
        fails.append("signing could not be exercised: " + p.stderr[-200:])
        break
    if got != [want(x) for x in PAYLOADS]:
        fails.append("with PYTHONHASHSEED=%d the canonical form or signature differs from the spec: %r" % (seed, got[0][0]))
        break
for seed in (1, 6, 7, 9):
    env = dict(os.environ, PYTHONHASHSEED=str(seed))
    p = subprocess.run([sys.executable, "-m", "unittest", "discover", "-q"], env=env, capture_output=True,
                       encoding="utf-8", errors="replace", timeout=60)
    if p.returncode != 0:
        fails.append("the tests fail with PYTHONHASHSEED=%d: %s" % (seed, p.stderr[-200:]))
        break
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
