import hashlib
import itertools
import os
import re
import subprocess
import sys

sys.path.insert(0, ".")
SHIPPED = {
    "README.md": "329945170860ff3f8c85296475eb4f586c53236c8797b3b303e1a206ea737907",
    "gateway.py": "e2c109e0a9ed09ffee7c3f2b652050a42b2a38fada410152ca8679178c2410d8",
    "test_payments.py": "6df05c725b9bcf5fcb2c5e1cd9987327fcf179fde0527a78237f3cdbf0ac39d1",
    "payments.py": None,
}
NOISE = ("__pycache__", ".pytest_cache", ".flynn-ab")


def h(p):
    return hashlib.sha256(open(p, "rb").read().replace(b"\r\n", b"\n")).hexdigest()


fails = []
for name, digest in SHIPPED.items():
    if not os.path.exists(name):
        fails.append(name + " was removed")
    elif digest and h(name) != digest:
        fails.append(name + " was edited; the gateway and its tests are the platform team's, so the fix goes in payments.py")
try:
    from gateway import Gateway
    from payments import pay

    bad = []
    total = 0
    # Every way the first seven gateway calls can go wrong, up to four at once.
    for faults in range(5):
        for where in itertools.combinations(range(7), faults):
            for modes in itertools.product(("lost", "late"), repeat=faults):
                script = ["ok"] * 7
                for i, m in zip(where, modes):
                    script[i] = m
                total += 1
                gateway, order = Gateway(script), {"id": 7, "cents": 1250}
                try:
                    pay(gateway, order)
                except Exception as e:
                    bad.append((script, "raised %r" % (e,)))
                    continue
                held, state = gateway.charges_for(7), order.get("state")
                if held > 1:
                    bad.append((script, "charged %d times" % held))
                elif state == "paid" and held != 1:
                    bad.append((script, "paid with no charge held"))
                elif state == "failed" and held != 0:
                    bad.append((script, "failed while the gateway holds the charge"))
                elif state == "unknown" and gateway.answered:
                    bad.append((script, "unknown although the gateway answered"))
                elif state not in ("paid", "failed", "unknown"):
                    bad.append((script, "state is %r" % (state,)))
                elif faults <= 2 and state != "paid":
                    bad.append((script, "ended %s with %d faults, which three attempts ride out" % (state, faults)))
    if bad:
        fails.append("%d of %d failure patterns end wrong, for example %s -> %s" % (len(bad), total, bad[0][0], bad[0][1]))
except Exception as e:
    fails.append("payments is not usable: %r" % (e,))
p = subprocess.run([sys.executable, "-m", "unittest"], capture_output=True, encoding="utf-8", errors="replace", timeout=60)
out = p.stdout + p.stderr
ran = re.search(r"Ran (\d+) tests?", out)
if p.returncode != 0 or not ran or int(ran.group(1)) < 2:
    fails.append("the project's own tests do not pass: " + out[-300:])
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
