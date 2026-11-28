import time

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
