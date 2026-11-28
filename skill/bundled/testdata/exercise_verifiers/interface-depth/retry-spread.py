import ast
import subprocess
import sys
import time

time.sleep = lambda s: None
sys.path.insert(0, ".")
fails = []


class Abort(BaseException):
    pass


try:
    import adjustments
    import invoices
    import ledger_client as lc
    import nightly_export
    import payouts
    import refunds
    import signup_credit

    state = {"calls": 0}

    def script(outcomes, then="ok"):
        state["calls"] = 0
        seq = list(outcomes)

        def transport(entry):
            state["calls"] += 1
            if state["calls"] > 50:
                raise Abort()
            out = seq.pop(0) if seq else then
            if out == "T":
                raise lc.TransientError("dropped")
            if out == "P":
                raise lc.PermanentError("rejected")
            return "r%d" % state["calls"]

        lc.TRANSPORT = transport

    inv = {"id": "i1", "cents": 100}
    callers = {
        "invoices.record_invoice": lambda: invoices.record_invoice(inv),
        "refunds.record_refund": lambda: refunds.record_refund(inv),
        "payouts.record_payout": lambda: payouts.record_payout(inv),
        "adjustments.record_adjustment": lambda: adjustments.record_adjustment(inv),
        "signup_credit.grant_signup_credit": lambda: signup_credit.grant_signup_credit("u1"),
        "nightly_export.export_day": lambda: nightly_export.export_day("d1", [{"id": "a", "cents": 1}, {"id": "b", "cents": 2}]),
    }
    for name, call in callers.items():
        script(["T", "T"])
        try:
            res = call()
            if name == "adjustments.record_adjustment" and res is not True:
                fails.append("%s parked the entry after two dropped requests; the ledger was reachable" % name)
        except BaseException as e:
            fails.append("%s fails on two dropped requests in a row: %r" % (name, e))
        script(["P"])
        try:
            call()
            fails.append("%s accepted an entry the ledger rejected" % name)
        except lc.PermanentError:
            if state["calls"] != 1:
                fails.append("%s sent a rejected entry %d times; a rejection is never repeated" % (name, state["calls"]))
        except BaseException as e:
            fails.append("%s on a rejected entry raised %r, want PermanentError" % (name, e))
        script([], then="T")
        try:
            res = call()
            if name == "adjustments.record_adjustment":
                if res is not False:
                    fails.append("adjustments must still park the entry when the ledger stays down")
            else:
                fails.append("%s returned while the ledger was down" % name)
        except lc.TransientError:
            pass
        except Abort:
            fails.append("%s never gives up while the ledger is down" % name)
        except BaseException as e:
            fails.append("%s while the ledger is down raised %r" % (name, e))
        if state["calls"] > 10 and "never gives up" not in "".join(fails):
            fails.append("%s sent one entry %d times while the ledger was down" % (name, state["calls"]))
    script(["T", "T"])
    try:
        lc.post({"kind": "probe"})
    except BaseException as e:
        fails.append("a new caller of ledger_client.post still has to write its own retry: %r" % (e,))
    for mod in ("refunds", "payouts", "signup_credit"):
        tree = ast.parse(open(mod + ".py", encoding="utf-8").read())
        if any(isinstance(n, (ast.Try, ast.While)) for n in ast.walk(tree)):
            fails.append("%s.py still carries its own retry loop" % mod)
    p = subprocess.run([sys.executable, "-m", "unittest", "discover", "-s", ".", "-p", "test_*.py"], capture_output=True, text=True)
    if p.returncode != 0:
        fails.append("the suite fails: " + p.stderr[-300:])
except Exception as e:
    fails.append("the ledger modules could not be exercised: %r" % (e,))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
