import importlib
import os
import sys

sys.path.insert(0, ".")
fails = []
path = os.path.join(os.getcwd(), "_probe_ledger.json")


def msg(mid, account, cents):
    return {"id": mid, "account": account, "cents": cents}


try:
    import consumer
    import ledger

    book = ledger.Ledger(path)
    for m in (msg("m1", "a1", 500), msg("m2", "a1", 300), msg("m1", "a1", 500)):
        consumer.handle(book, m)
    got = book.balance("a1")
    if got != 800:
        fails.append("m1 delivered twice in one run left a1 at %r, want 800" % (got,))
    importlib.reload(ledger)
    importlib.reload(consumer)
    book = ledger.Ledger(path)
    consumer.handle(book, msg("m1", "a1", 500))
    got = book.balance("a1")
    if got != 800:
        fails.append("after a restart the redelivered m1 left a1 at %r, want 800; what was applied has to outlive the process" % (got,))
    consumer.handle(book, msg("m3", "a1", 100))
    got = book.balance("a1")
    if got != 900:
        fails.append("a new message m3 after the restart left a1 at %r, want 900" % (got,))
    consumer.handle(book, msg("m4", "a1", 500))
    got = book.balance("a1")
    if got != 1400:
        fails.append("m4 has m1's account and amount and is a different payment; it left a1 at %r, want 1400" % (got,))
except Exception as e:
    fails.append("the consumer could not be exercised: %r" % (e,))
for f in fails:
    print(f, file=sys.stderr)
sys.exit(1 if fails else 0)
