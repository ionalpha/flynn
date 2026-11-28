import importlib
import os
import subprocess
import sys
import uuid

sys.path.insert(0, ".")
fails = []
try:
    def load():
        # A new process: every module of the project starts from what is on disk.
        here = os.path.abspath(".") + os.sep
        for name, module in list(sys.modules.items()):
            if name != "__main__" and os.path.abspath(getattr(module, "__file__", None) or os.sep).startswith(here):
                del sys.modules[name]
        return importlib.import_module("handlers"), importlib.import_module("ledger")

    def event(eid, delivery, account):
        return {"id": eid, "delivery": delivery, "type": "invoice.paid",
                "data": {"account": account, "amount_cents": 500}}

    def send(handlers, e):
        try:
            return handlers.handle_event(e)
        except Exception:
            return 500

    def ok(status):
        return isinstance(status, int) and 200 <= status < 300

    run = uuid.uuid4().hex
    handlers, ledger = load()
    a, e1 = "acct_a_" + run, "evt_a_" + run
    first, second = send(handlers, event(e1, 1, a)), send(handlers, event(e1, 2, a))
    if not ok(first) or not ok(second):
        fails.append("deliveries answered %r then %r; anything but 2xx is redelivered" % (first, second))
    if ledger.balance(a) != 500:
        fails.append("one event delivered twice left a balance of %r" % ledger.balance(a))
    handlers, ledger = load()
    send(handlers, event(e1, 3, a))
    if ledger.balance(a) != 500:
        fails.append("a redelivery after a restart credited again: balance %r" % ledger.balance(a))
    send(handlers, event("evt_b_" + run, 1, a))
    if ledger.balance(a) != 1000:
        fails.append("a second, different event did not credit: balance %r" % ledger.balance(a))

    b, e3 = "acct_c_" + run, "evt_c_" + run
    ledger.DOWN = True
    status = send(handlers, event(e3, 1, b))
    if ok(status):
        fails.append("the ledger was down and the event was acknowledged with %r, so it is never sent again" % status)
    ledger.DOWN = False
    send(handlers, event(e3, 2, b))
    send(handlers, event(e3, 3, b))
    if ledger.balance(b) != 500:
        fails.append("after an outage and redelivery the balance is %r, want 500" % ledger.balance(b))
    tests = subprocess.run([sys.executable, "-m", "unittest", "discover", "-q"], capture_output=True, text=True)
    if tests.returncode != 0:
        fails.append("the tests fail: " + tests.stderr[-1500:])
except Exception as e:
    fails.append("verifier could not run: %r" % (e,))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
