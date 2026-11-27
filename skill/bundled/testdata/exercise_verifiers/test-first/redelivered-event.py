import os
import shutil
import subprocess
import sys

sys.path.insert(0, ".")
sys.dont_write_bytecode = True
fails = []


def suite(d="."):
    p = subprocess.run([sys.executable, "-B", "-m", "unittest", "discover", "-s", ".", "-p", "test*.py"],
                       cwd=d, capture_output=True, encoding="utf-8", errors="replace", timeout=60)
    return p.returncode, p.stderr


rc, out = suite()
if rc != 0:
    fails.append("the project's tests fail: " + out[-300:])
try:
    from inbox import Seen
    from ledger import Ledger, LedgerUnavailable
    from webhook import handle

    def deliver(ledger, seen, event):
        try:
            return handle(dict(event), ledger, seen)
        except LedgerUnavailable:
            return False

    class Flaky(Ledger):
        down = 1

        def credit(self, *a, **k):
            if self.down:
                self.down -= 1
                raise LedgerUnavailable()
            return super().credit(*a, **k)

    e1 = {"id": "e1", "account": "a", "cents": 500}
    ledger, seen = Ledger(), Seen()
    acks = [deliver(ledger, seen, e1) for _ in range(3)]
    if ledger.balance("a") != 500 or not all(acks):
        fails.append("three deliveries: balance %r, answers %r" % (ledger.balance("a"), acks))
    deliver(ledger, seen, dict(e1, id="e2"))
    if ledger.balance("a") != 1000:
        fails.append("a second payment of the same amount was dropped")
    ledger, seen = Flaky(), Seen()
    for _ in range(5):
        if deliver(ledger, seen, e1):
            break
    if ledger.balance("a") != 500:
        fails.append("the ledger was down on the first delivery; after the retries the balance is %r" % ledger.balance("a"))
except Exception as e:
    fails.append("the webhook is not usable: %r" % (e,))
if not fails:
    shutil.copytree(".", "_orig", ignore=shutil.ignore_patterns("_orig", "__pycache__", ".git", ".flynn-ab"))
    with open(os.path.join("_orig", "webhook.py"), "w", newline="\n", encoding="utf-8") as f:
        f.write('def handle(event, ledger, seen):\n    """Apply one payment event. True tells the provider to stop sending it."""\n    ledger.credit(event["account"], event["cents"])\n    return True\n')
    if suite("_orig")[0] == 0:
        fails.append("the tests still pass on the code as it was, which credits every delivery")
    shutil.rmtree("_orig")
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
