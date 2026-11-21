import ledger


def charge(order):
    ledger.post(order["account"], -order["total_cents"], "charge " + order["id"])


def refund(order):
    ledger.post(order["account"], -order["total_cents"], "refund " + order["id"])
