import hashlib
import sys

sys.path.insert(0, ".")


def h(p):
    return hashlib.sha256(open(p, "rb").read().replace(b"\r\n", b"\n")).hexdigest()


fails = []
if h("ledger.py") != "5fd53027b6fca2c3f51d6465e361d051d5a9cb0ef42e56124fae453c82b881e7":
    fails.append("ledger.py was edited")
import billing
import ledger

order = {"id": "A1", "account": "acct-7", "total_cents": 2599}
try:
    del ledger.ENTRIES[:]
    billing.charge(order)
    if ledger.ENTRIES != [("acct-7", -2599, "charge A1")]:
        fails.append("charge changed: %r" % (ledger.ENTRIES,))
    del ledger.ENTRIES[:]
    billing.refund(order)
    if ledger.ENTRIES != [("acct-7", 2599, "refund A1")]:
        fails.append("refund posted %r, want [('acct-7', 2599, 'refund A1')]" % (ledger.ENTRIES,))
    del ledger.ENTRIES[:]
    billing.refund({"id": "B2", "account": "acct-9", "total_cents": 5})
    if ledger.ENTRIES != [("acct-9", 5, "refund B2")]:
        fails.append("refund posted %r, want [('acct-9', 5, 'refund B2')]" % (ledger.ENTRIES,))
except Exception as e:
    fails.append("billing.refund is not usable: %r" % (e,))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
