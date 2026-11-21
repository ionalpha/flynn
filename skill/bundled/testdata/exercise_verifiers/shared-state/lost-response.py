import hashlib
import os
import shutil
import subprocess
import sys
import time

sys.path.insert(0, ".")
time.sleep = lambda s: None
fails = []
ORIGINAL = '''import time

from vendor.gateway import Declined


def with_retry(fn, attempts=3):
    for attempt in range(attempts):
        try:
            return fn()
        except Exception:
            if attempt == attempts - 1:
                raise
            time.sleep(0.2 * (attempt + 1))


def settle(gateway, invoices):
    """Charge each invoice {id, account, cents}; returns {"paid": [ids], "declined": [ids]}."""
    paid, declined = [], []
    for inv in invoices:
        try:
            with_retry(lambda: gateway.charge(inv["account"], inv["cents"]))
            paid.append(inv["id"])
        except Declined:
            declined.append(inv["id"])
    return {"paid": paid, "declined": declined}
'''


def h(p):
    return hashlib.sha256(open(p, "rb").read().replace(b"\r\n", b"\n")).hexdigest()[:16]


try:
    if h("vendor/gateway.py") != "6b55ff30071361d7":
        fails.append("vendor/gateway.py changed; it is the shared client and the fix has to work with it as it is")
except OSError:
    fails.append("vendor/gateway.py is gone")
INV = [{"id": "inv-1", "account": "a1", "cents": 500}, {"id": "inv-2", "account": "a1", "cents": 500}]
try:
    import billing
    from vendor.gateway import ConnectionRefused, Gateway, ReadTimeout

    def go(script, invoices, gw=None):
        gw = gw or Gateway(script)
        try:
            return gw, billing.settle(gw, invoices), None
        except Exception as e:
            return gw, None, e

    gw, res, err = go(["lose"], INV[:1])
    if err or res != {"paid": ["inv-1"], "declined": []}:
        fails.append("a lost response on inv-1 gave %r %r; it should settle" % (res, err))
    if len(gw.applied) != 1:
        fails.append("a lost response on inv-1 charged the account %d times, want 1" % len(gw.applied))
    gw, res, err = go(["lose", "lose", "ok"], INV[:1])
    if len(gw.applied) != 1:
        fails.append("two lost responses charged the account %d times, want 1" % len(gw.applied))
    gw, res, err = go(["refuse", "ok"], INV[:1])
    if err or len(gw.applied) != 1:
        fails.append("a refused connection then success left %d charges and %r; it should retry and settle" % (len(gw.applied), err))
    gw, res, err = go(["decline"], INV[:1])
    if err or res != {"paid": [], "declined": ["inv-1"]} or len(gw.calls) != 1:
        fails.append("a declined charge made %d calls and gave %r %r; it is reported after one call" % (len(gw.calls), res, err))
    gw, res, err = go(["garbled"], INV[:1])
    if not isinstance(err, ValueError) or len(gw.calls) != 1:
        fails.append("an unclassified error made %d calls and gave %r; it must not be retried" % (len(gw.calls), err))
    gw, res, err = go(["refuse"] * 50, INV[:1])
    if not isinstance(err, ConnectionRefused) or len(gw.calls) > 6:
        fails.append("a gateway that refuses forever got %d calls and %r; retry is bounded and the last error surfaces" % (len(gw.calls), err))
    gw, res, err = go(["lose", "lose"], INV)
    if err or len(gw.applied) != 2:
        fails.append("two invoices of one account and amount, each with a lost response, made %d charges, want 2" % len(gw.applied))
    gw, res, err = go(["lose"] * 20, INV[:1])
    if not isinstance(err, ReadTimeout) or len(gw.applied) != 1:
        fails.append("a response lost every time made %d charges and gave %r; one charge and the timeout surfaced" % (len(gw.applied), err))
    first = list(gw.calls)
    gw.script = []
    billing.settle(gw, INV[:1])
    if len(gw.applied) != 1 or None in gw.calls or len(set(first)) != 1:
        fails.append("settling the same invoice again after a failure made %d charges; the key comes from the invoice, the same on every attempt and every run" % len(gw.applied))
except Exception as e:
    fails.append("billing.settle could not be exercised: %r" % (e,))


def run(d):
    p = subprocess.run([sys.executable, "-m", "unittest", "discover", "-s", ".", "-p", "test*.py"],
                       cwd=d, capture_output=True, encoding="utf-8", errors="replace", timeout=60)
    return p.returncode, p.stdout + p.stderr


rc, out = run(".")
if rc != 0:
    fails.append("the tests fail on the result: " + out[-300:])
d = "_mut_original"
shutil.copytree(".", d, ignore=shutil.ignore_patterns("_mut_*", "__pycache__", ".git", ".flynn-ab"))
with open(os.path.join(d, "billing.py"), "w", newline="\n", encoding="utf-8") as f:
    f.write(ORIGINAL)
rc, out = run(d)
shutil.rmtree(d, ignore_errors=True)
if rc == 0 or "AssertionError" not in out:
    fails.append("the tests do not fail with an assertion on the original settle; the fix ships with a test that fails without it")
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
