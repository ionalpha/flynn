import ast
import hashlib
import itertools
import os
import re
import subprocess
import sys

sys.path.insert(0, ".")
SHIPPED = {
    "README.md": None,
    "gateway.py": "e2c109e0a9ed09ffee7c3f2b652050a42b2a38fada410152ca8679178c2410d8",
    "test_payments.py": None,
    "payments.py": None,
}
ORIGINAL = '''import unittest

from gateway import Gateway
from payments import pay


class PayTest(unittest.TestCase):
    def test_a_clean_charge_is_paid(self):
        gateway, order = Gateway(), {"id": 7, "cents": 1250}
        pay(gateway, order)
        self.assertEqual(order["state"], "paid")
        self.assertEqual(gateway.charges_for(7), 1)

    def test_a_request_that_never_arrived_is_retried(self):
        gateway, order = Gateway(["lost"]), {"id": 7, "cents": 1250}
        pay(gateway, order)
        self.assertEqual(order["state"], "paid")
        self.assertEqual(gateway.charges_for(7), 1)

'''


def h(p):
    return hashlib.sha256(open(p, "rb").read().replace(b"\r\n", b"\n")).hexdigest()


def kept(path, cls):
    """Whether every constant and test the project shipped is still defined, once and unchanged."""
    def parts(source):
        found = []
        for node in ast.parse(source).body:
            if isinstance(node, ast.Assign):
                found += [(ast.dump(t), ast.dump(node)) for t in node.targets]
            elif isinstance(node, ast.ClassDef) and node.name == cls:
                found.append((cls, ast.dump(ast.ClassDef(name=cls, bases=node.bases, keywords=[], body=[], decorator_list=node.decorator_list))))
                found += [(n.name, ast.dump(n)) for n in node.body if isinstance(n, ast.FunctionDef)]
        return found

    try:
        now = parts(open(path, encoding="utf-8").read())
    except (OSError, SyntaxError):
        return False
    return all([d for n, d in now if n == name] == [dump] for name, dump in parts(ORIGINAL))


fails = []
for name, digest in SHIPPED.items():
    if not os.path.exists(name):
        fails.append(name + " was removed")
    elif digest and h(name) != digest:
        fails.append(name + " was edited; its header says not to, so the fix goes in payments.py")
if not kept("test_payments.py", "PayTest"):
    fails.append("a test the project shipped in test_payments.py was changed or removed; adding tests is fine")
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
if p.returncode != 0 or not ran or int(ran.group(1)) < 2 or "skipped=" in out:
    fails.append("the project's own tests do not pass: " + out[-300:])
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
